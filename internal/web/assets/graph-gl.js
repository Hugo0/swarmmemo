// graph-gl.js: the /graph renderer. WebGL2, no dependencies.
//
// Points and links are instanced quads; positions are fixed (the server lays
// the map out), so a frame only moves the camera and runs time-based effects
// in the shaders: reveal (a point blooms out of its parent), replay flashes,
// links that light up, and particles flowing along bridges. In the dark theme
// the scene is drawn additively into a half-float target, bloomed (bright
// pass, two blur octaves) and tone-mapped over a faint parallax starfield; in
// the light theme it is composited over paper, without bloom. Rendering is
// dirty-flagged: an idle map with no running effect draws nothing.
//
// Picking and lasso are on the CPU (screen-space distance and polygon tests
// over the drawn points), so the GPU never stalls on a read-back.

export const STYLE = { halo: 0, core: 1, msg: 2, ring: 3, infra: 4, cloud: 5 };
export const FLASH = 8; // added to a style: a replay flash on reveal

const QUAD = new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]);
const SEG = 24; // segments per link curve
const REVEAL = 0.75; // seconds a point takes to bloom in

const VS_POINT = `#version 300 es
precision highp float;
layout(location=0) in vec2 corner;
layout(location=1) in vec2 pos;
layout(location=2) in vec2 origin;
layout(location=3) in float radius;
layout(location=4) in vec4 color;
layout(location=5) in float style;
layout(location=6) in float birth;
layout(location=7) in float hl;
uniform vec3 cam; uniform vec2 view; uniform float time; uniform float dpr; uniform float dim; uniform float motion;
out vec2 uv; out vec4 col; out float st; out float px; out float flash;
void main(){
  float s = mod(style, 8.0);
  float age = (time - birth) * motion;
  float t = clamp(age / ${REVEAL.toFixed(2)}, 0.0, 1.0);
  float e = 1.0 - pow(1.0 - t, 3.0);
  vec2 p = mix(origin, pos, e);
  float rpx = radius * cam.z;
  float minpx = s == 2.0 ? 0.6 : (s == 0.0 || s == 5.0 ? 0.0 : 1.4);
  rpx = max(rpx, minpx * dpr);
  flash = style >= 8.0 ? exp(-max(age, 0.0) * 1.4) : 0.0;
  rpx *= mix(0.35, 1.0, e) * (1.0 + flash * 2.2);
  if (s == 0.0 || s == 5.0) rpx *= 1.0;
  vec2 sp = (p - cam.xy) * cam.z + corner * (rpx + 1.5 * dpr);
  gl_Position = vec4(sp.x / (view.x * 0.5), -sp.y / (view.y * 0.5), 0.0, 1.0);
  uv = corner * (rpx + 1.5 * dpr) / max(rpx, 1e-3);
  float g = hl > 0.5 ? 1.0 : dim;
  col = vec4(color.rgb, color.a * e * g);
  if (age < 0.0) col.a = 0.0;
  st = s; px = rpx;
}`;

const FS_POINT = `#version 300 es
precision highp float;
in vec2 uv; in vec4 col; in float st; in float px; in float flash;
uniform float additive;
out vec4 o;
void main(){
  float d = length(uv);
  float aa = 1.5 / max(px, 1.0);
  float a; vec3 c = col.rgb; float glow = 0.0;
  if (st == 0.0) { // halo: a soft gaussian, additive in the dark
    a = exp(-3.2 * d * d) * (1.0 - smoothstep(0.8, 1.0, d)); glow = 1.0;
  } else if (st == 5.0) { // cloud: a community seen from afar
    a = pow(max(0.0, 1.0 - d), 1.6) * 0.9 + 0.12 * (1.0 - smoothstep(0.96 - aa, 0.98, d)) * smoothstep(0.85, 0.97, d); glow = 0.6;
  } else if (st == 3.0) { // ring: a room or board, context not a participant
    a = (1.0 - smoothstep(aa, 2.0 * aa, abs(d - 0.92))) * 0.9 + 0.06 * (1.0 - smoothstep(0.9, 0.92, d));
  } else if (st == 4.0) { // infra: a diamond with a bright rim
    float dd = abs(uv.x) + abs(uv.y);
    a = 1.0 - smoothstep(0.86 - aa, 0.86 + aa, dd);
    c = mix(c, vec3(1.0), 0.35 * smoothstep(0.55, 0.86, dd));
  } else { // core and message: a lit disc
    a = 1.0 - smoothstep(1.0 - aa, 1.0, d);
    c *= mix(1.18, 0.82, d * d);
    c = mix(c, vec3(1.0), 0.18 * (1.0 - smoothstep(0.0, 0.5, length(uv - vec2(-0.32, -0.32)))));
  }
  a *= col.a;
  a += flash * exp(-2.2 * d * d) * 0.9;
  c = mix(c, vec3(1.0), clamp(flash, 0.0, 1.0) * 0.6);
  if (a <= 0.002) discard;
  // Premultiplied; alpha 0 makes a fragment add light instead of covering.
  o = vec4(c * a, glow * additive > 0.5 ? 0.0 : a);
}`;

const VS_LINK = `#version 300 es
precision highp float;
layout(location=0) in vec2 ts;  // t along the curve, side -1..1
layout(location=1) in vec4 ab;  // two ends in world space
layout(location=2) in vec4 color;
layout(location=3) in vec4 shape; // width px, bend, dash (0/1), birth
layout(location=4) in float hl;
uniform vec3 cam; uniform vec2 view; uniform float time; uniform float dpr; uniform float dim; uniform float motion;
out float vt; out float vs; out vec4 col; out float len; out float dash; out float prog; out float w;
vec2 scr(vec2 p){ return (p - cam.xy) * cam.z; }
void main(){
  vec2 a = scr(ab.xy), b = scr(ab.zw);
  vec2 d = b - a; float L = max(length(d), 1e-3);
  vec2 n = vec2(-d.y, d.x) / L;
  vec2 c = (a + b) * 0.5 + n * L * shape.y;
  float t = ts.x;
  vec2 p = (1.0 - t) * (1.0 - t) * a + 2.0 * (1.0 - t) * t * c + t * t * b;
  vec2 tg = 2.0 * (1.0 - t) * (c - a) + 2.0 * t * (b - c);
  vec2 nn = normalize(vec2(-tg.y, tg.x) + 1e-6);
  w = max(shape.x * dpr, 0.6 * dpr);
  p += nn * ts.y * (w * 0.5 + 1.5 * dpr);
  gl_Position = vec4(p.x / (view.x * 0.5), -p.y / (view.y * 0.5), 0.0, 1.0);
  vt = t; vs = ts.y * (w * 0.5 + 1.5 * dpr); len = L; dash = shape.z;
  prog = clamp((time - shape.w) * motion / 0.9, -1.0, 1.0);
  float g = hl > 0.5 ? 1.0 : dim;
  col = vec4(color.rgb, color.a * g);
}`;

const FS_LINK = `#version 300 es
precision highp float;
in float vt; in float vs; in vec4 col; in float len; in float dash; in float prog; in float w;
uniform float additive; uniform float dpr;
out vec4 o;
void main(){
  if (prog < 0.0 || vt > prog + 0.001) discard;
  float edge = 1.0 - smoothstep(w * 0.5 - 0.5 * dpr, w * 0.5 + 1.0 * dpr, abs(vs));
  float taper = smoothstep(0.0, 0.08, vt) * smoothstep(1.0, 0.92, vt);
  float a = col.a * edge * mix(0.35, 1.0, taper);
  if (dash > 0.5) { float k = fract(vt * len / (10.0 * dpr)); a *= 1.0 - smoothstep(0.5, 0.62, k); }
  float head = prog < 1.0 ? exp(-pow((vt - prog) * 14.0, 2.0)) : 0.0; // the lighting front
  vec3 c = mix(col.rgb, vec3(1.0), head * 0.7);
  a += head * edge * 0.9;
  if (a <= 0.002) discard;
  o = vec4(c * a, additive > 0.5 ? a * 0.25 : a);
}`;

const VS_PART = `#version 300 es
precision highp float;
layout(location=0) in vec2 corner;
layout(location=1) in vec4 ab;
layout(location=2) in vec4 color;
layout(location=3) in vec4 motionv; // bend, speed (cycles/s), phase, size px
uniform vec3 cam; uniform vec2 view; uniform float time; uniform float dpr;
out vec2 uv; out vec4 col;
vec2 scr(vec2 p){ return (p - cam.xy) * cam.z; }
void main(){
  vec2 a = scr(ab.xy), b = scr(ab.zw);
  vec2 d = b - a; float L = max(length(d), 1e-3);
  vec2 n = vec2(-d.y, d.x) / L;
  vec2 c = (a + b) * 0.5 + n * L * motionv.x;
  float t = fract(motionv.z + time * motionv.y);
  vec2 p = (1.0 - t) * (1.0 - t) * a + 2.0 * (1.0 - t) * t * c + t * t * b;
  float s = motionv.w * dpr * (0.6 + 0.4 * sin(3.14159 * t));
  p += corner * s;
  gl_Position = vec4(p.x / (view.x * 0.5), -p.y / (view.y * 0.5), 0.0, 1.0);
  uv = corner; col = color; col.a *= smoothstep(0.0, 0.08, t) * smoothstep(1.0, 0.9, t);
}`;

const FS_PART = `#version 300 es
precision highp float;
in vec2 uv; in vec4 col; uniform float additive;
out vec4 o;
void main(){
  float a = exp(-4.0 * dot(uv, uv)) * col.a;
  if (a <= 0.002) discard;
  o = vec4(mix(col.rgb, vec3(1.0), 0.4) * a, additive > 0.5 ? 0.0 : a);
}`;

const VS_FULL = `#version 300 es
layout(location=0) in vec2 corner;
out vec2 uv;
void main(){ uv = corner * 0.5 + 0.5; gl_Position = vec4(corner, 0.0, 1.0); }`;

const FS_BRIGHT = `#version 300 es
precision highp float;
in vec2 uv; uniform sampler2D src; uniform vec2 texel;
out vec4 o;
void main(){
  vec3 c = vec3(0.0);
  for (int i = 0; i < 4; i++) { vec2 off = vec2(i % 2 == 0 ? -0.5 : 0.5, i < 2 ? -0.5 : 0.5) * texel; c += texture(src, uv + off).rgb; }
  c *= 0.25;
  float l = max(c.r, max(c.g, c.b));
  float k = clamp((l - 0.35) / 0.5, 0.0, 1.0);
  o = vec4(c * k * k, 1.0);
}`;

const FS_BLUR = `#version 300 es
precision highp float;
in vec2 uv; uniform sampler2D src; uniform vec2 dir;
out vec4 o;
void main(){
  vec3 c = texture(src, uv).rgb * 0.227027;
  c += (texture(src, uv + dir * 1.3846).rgb + texture(src, uv - dir * 1.3846).rgb) * 0.316216;
  c += (texture(src, uv + dir * 3.2308).rgb + texture(src, uv - dir * 3.2308).rgb) * 0.070270;
  o = vec4(c, 1.0);
}`;

const FS_COMPOSE = `#version 300 es
precision highp float;
in vec2 uv; uniform sampler2D scene; uniform sampler2D bloom1; uniform sampler2D bloom2;
uniform vec3 bg; uniform float dark; uniform float bloomOn; uniform vec2 view; uniform vec3 cam; uniform float dpr;
out vec4 o;
float h(vec2 p){ return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453); }
float stars(vec2 frag, float cell, float par, float dens){
  vec2 q = frag / (cell * dpr) + cam.xy * cam.z * par / (cell * dpr);
  vec2 id = floor(q), f = fract(q);
  float r = h(id);
  if (r < dens) return 0.0;
  vec2 c = vec2(h(id + 7.1), h(id + 3.3)) * 0.8 + 0.1;
  float d = length(f - c) * cell;
  return smoothstep(1.4, 0.0, d) * (r - dens) / (1.0 - dens);
}
void main(){
  vec4 s = texture(scene, uv);
  vec3 col;
  vec2 frag = uv * view;
  if (dark > 0.5) {
    float st = stars(frag, 28.0, 0.02, 0.965) * 0.55 + stars(frag + 91.0, 61.0, 0.05, 0.94) * 0.4;
    vec3 sky = bg + vec3(0.6, 0.65, 0.8) * st * 0.35;
    float v = length(uv - 0.5); sky *= 1.0 - 0.35 * v * v;
    vec3 light = s.rgb;
    if (bloomOn > 0.5) light += texture(bloom1, uv).rgb * 0.9 + texture(bloom2, uv).rgb * 0.7;
    light = 1.0 - exp(-light * 1.25); // tone map, so dense regions glow instead of clipping
    col = sky * (1.0 - s.a) + light;
  } else {
    float grain = stars(frag, 34.0, 0.02, 0.975) * 0.05;
    vec3 paper = bg - grain;
    col = paper * (1.0 - s.a) + s.rgb;
    if (bloomOn > 0.5) col += texture(bloom1, uv).rgb * 0.12;
  }
  o = vec4(col, 1.0);
}`;

function compile(gl, vs, fs) {
  const p = gl.createProgram();
  for (const [type, src] of [[gl.VERTEX_SHADER, vs], [gl.FRAGMENT_SHADER, fs]]) {
    const s = gl.createShader(type); gl.shaderSource(s, src); gl.compileShader(s);
    if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) throw new Error('shader: ' + gl.getShaderInfoLog(s));
    gl.attachShader(p, s);
  }
  gl.linkProgram(p);
  if (!gl.getProgramParameter(p, gl.LINK_STATUS)) throw new Error('program: ' + gl.getProgramInfoLog(p));
  const u = {}; const n = gl.getProgramParameter(p, gl.ACTIVE_UNIFORMS);
  for (let i = 0; i < n; i++) { const name = gl.getActiveUniform(p, i).name; u[name] = gl.getUniformLocation(p, name); }
  return { p, u };
}

const ease = (t) => t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;

export class Renderer {
  constructor(canvas, opt = {}) {
    this.canvas = canvas;
    const gl = canvas.getContext('webgl2', { antialias: false, alpha: false, premultipliedAlpha: false, preserveDrawingBuffer: false, powerPreference: 'high-performance' });
    if (!gl) throw new Error('WebGL2 is not available');
    this.gl = gl;
    this.half = !!gl.getExtension('EXT_color_buffer_float') || !!gl.getExtension('EXT_color_buffer_half_float');
    this.reduced = !!opt.reducedMotion;
    this.dpr = Math.min(2, opt.pixelRatio || devicePixelRatio || 1);
    this.prog = { point: compile(gl, VS_POINT, FS_POINT), link: compile(gl, VS_LINK, FS_LINK), part: compile(gl, VS_PART, FS_PART),
      bright: compile(gl, VS_FULL, FS_BRIGHT), blur: compile(gl, VS_FULL, FS_BLUR), compose: compile(gl, VS_FULL, FS_COMPOSE) };
    this.quad = this.buffer(QUAD);
    const strip = new Float32Array((SEG + 1) * 4);
    for (let i = 0; i <= SEG; i++) strip.set([i / SEG, -1, i / SEG, 1], i * 4);
    this.strip = this.buffer(strip);
    this.full = this.buffer(QUAD);
    this.pts = { n: 0, buf: null, pos: new Float32Array(0), r: new Float32Array(0), style: new Uint8Array(0), hl: null };
    this.lks = { n: 0, buf: null, ab: new Float32Array(0), hl: null };
    this.parts = { n: 0, buf: null };
    this.theme = { bg: [0.06, 0.06, 0.06], dark: true };
    this.cam = { x: 2048, y: 2048, z: 0.2 };
    this.tween = null; this.spring = null;
    this.dirty = true; this.animUntil = 0; this.dim = 1;
    this.listeners = {};
    this.targets = null;
    this.resize();
    this.bindInput();
    this.vaos();
  }

  buffer(data, usage) { const gl = this.gl, b = gl.createBuffer(); gl.bindBuffer(gl.ARRAY_BUFFER, b); gl.bufferData(gl.ARRAY_BUFFER, data, usage || gl.STATIC_DRAW); return b; }

  vaos() {
    const gl = this.gl;
    this.vao = { point: gl.createVertexArray(), link: gl.createVertexArray(), part: gl.createVertexArray(), full: gl.createVertexArray() };
    gl.bindVertexArray(this.vao.full);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.full); gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);
  }

  // ---- targets: scene (HDR when possible) and two bloom octaves ----
  target(w, h) {
    const gl = this.gl, t = gl.createTexture();
    gl.bindTexture(gl.TEXTURE_2D, t);
    gl.texImage2D(gl.TEXTURE_2D, 0, this.half ? gl.RGBA16F : gl.RGBA8, w, h, 0, gl.RGBA, this.half ? gl.HALF_FLOAT : gl.UNSIGNED_BYTE, null);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR); gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE); gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
    const f = gl.createFramebuffer(); gl.bindFramebuffer(gl.FRAMEBUFFER, f);
    gl.framebufferTexture2D(gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, t, 0);
    if (gl.checkFramebufferStatus(gl.FRAMEBUFFER) !== gl.FRAMEBUFFER_COMPLETE && this.half) { this.half = false; gl.deleteTexture(t); gl.deleteFramebuffer(f); return this.target(w, h); }
    return { t, f, w, h };
  }
  resize() {
    const c = this.canvas, w = Math.max(1, Math.round(c.clientWidth * this.dpr)), h = Math.max(1, Math.round(c.clientHeight * this.dpr));
    if (c.width === w && c.height === h && this.targets) return false;
    c.width = w; c.height = h;
    const gl = this.gl;
    if (this.targets) for (const x of Object.values(this.targets)) { gl.deleteTexture(x.t); gl.deleteFramebuffer(x.f); }
    const hw = Math.max(1, w >> 1), hh = Math.max(1, h >> 1), qw = Math.max(1, w >> 2), qh = Math.max(1, h >> 2);
    this.targets = { scene: this.target(w, h), a1: this.target(hw, hh), b1: this.target(hw, hh), a2: this.target(qw, qh), b2: this.target(qw, qh) };
    this.dirty = true;
    return true;
  }
  get width() { return this.canvas.clientWidth; }
  get height() { return this.canvas.clientHeight; }

  setTheme(t) { this.theme = t; this.dirty = true; }

  // ---- data ----
  // points: { x, y, ox, oy, r, rgba, style, birth } typed arrays, one entry per point.
  setPoints(p) {
    const gl = this.gl, n = p.x.length;
    const data = new Float32Array(n * 12);
    for (let i = 0; i < n; i++) {
      const o = i * 12;
      data[o] = p.x[i]; data[o + 1] = p.y[i]; data[o + 2] = p.ox[i]; data[o + 3] = p.oy[i]; data[o + 4] = p.r[i];
      data[o + 5] = p.rgba[4 * i]; data[o + 6] = p.rgba[4 * i + 1]; data[o + 7] = p.rgba[4 * i + 2]; data[o + 8] = p.rgba[4 * i + 3];
      data[o + 9] = p.style[i]; data[o + 10] = p.birth[i]; data[o + 11] = 1;
    }
    if (this.pts.buf) gl.deleteBuffer(this.pts.buf);
    if (this.pts.hlBuf) gl.deleteBuffer(this.pts.hlBuf);
    this.pts = { n, buf: this.buffer(data), x: p.x, y: p.y, r: p.r, style: p.style, birth: p.birth, hlBuf: this.buffer(new Float32Array(n).fill(1), gl.DYNAMIC_DRAW) };
    gl.bindVertexArray(this.vao.point);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.quad); gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(0, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.pts.buf);
    const S = 48, at = (loc, size, off) => { gl.enableVertexAttribArray(loc); gl.vertexAttribPointer(loc, size, gl.FLOAT, false, S, off); gl.vertexAttribDivisor(loc, 1); };
    at(1, 2, 0); at(2, 2, 8); at(3, 1, 16); at(4, 4, 20); at(5, 1, 36); at(6, 1, 40);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.pts.hlBuf); gl.enableVertexAttribArray(7); gl.vertexAttribPointer(7, 1, gl.FLOAT, false, 4, 0); gl.vertexAttribDivisor(7, 1);
    gl.bindVertexArray(null);
    let last = 0; const now = this.now();
    for (let i = 0; i < n; i++) if (p.birth[i] > last) last = p.birth[i];
    this.animate(Math.max(0, last - now) + REVEAL + 0.1);
    this.dim = 1; this.dirty = true;
  }
  // links: { ax, ay, bx, by, rgba, width, bend, dash, birth } typed arrays.
  setLinks(l) {
    const gl = this.gl, n = l.ax.length;
    const data = new Float32Array(n * 12);
    let last = 0;
    for (let i = 0; i < n; i++) {
      const o = i * 12;
      data[o] = l.ax[i]; data[o + 1] = l.ay[i]; data[o + 2] = l.bx[i]; data[o + 3] = l.by[i];
      data[o + 4] = l.rgba[4 * i]; data[o + 5] = l.rgba[4 * i + 1]; data[o + 6] = l.rgba[4 * i + 2]; data[o + 7] = l.rgba[4 * i + 3];
      data[o + 8] = l.width[i]; data[o + 9] = l.bend[i]; data[o + 10] = l.dash[i]; data[o + 11] = l.birth[i];
      if (l.birth[i] > last) last = l.birth[i];
    }
    if (this.lks.buf) gl.deleteBuffer(this.lks.buf);
    if (this.lks.hlBuf) gl.deleteBuffer(this.lks.hlBuf);
    this.lks = { n, buf: this.buffer(data), l, hlBuf: this.buffer(new Float32Array(n).fill(1), gl.DYNAMIC_DRAW) };
    gl.bindVertexArray(this.vao.link);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.strip); gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(0, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.lks.buf);
    const S = 48, at = (loc, size, off) => { gl.enableVertexAttribArray(loc); gl.vertexAttribPointer(loc, size, gl.FLOAT, false, S, off); gl.vertexAttribDivisor(loc, 1); };
    at(1, 4, 0); at(2, 4, 16); at(3, 4, 32);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.lks.hlBuf); gl.enableVertexAttribArray(4); gl.vertexAttribPointer(4, 1, gl.FLOAT, false, 4, 0); gl.vertexAttribDivisor(4, 1);
    gl.bindVertexArray(null);
    this.animate(Math.max(0, last - this.now()) + 1.0);
    this.dirty = true;
  }
  // particles: { ax, ay, bx, by, rgba, bend, speed, phase, size } typed arrays.
  setParticles(q) {
    const gl = this.gl, n = this.reduced ? 0 : q.ax.length;
    const data = new Float32Array(n * 12);
    for (let i = 0; i < n; i++) {
      const o = i * 12;
      data.set([q.ax[i], q.ay[i], q.bx[i], q.by[i], q.rgba[4 * i], q.rgba[4 * i + 1], q.rgba[4 * i + 2], q.rgba[4 * i + 3], q.bend[i], q.speed[i], q.phase[i], q.size[i]], o);
    }
    if (this.parts.buf) gl.deleteBuffer(this.parts.buf);
    this.parts = { n, buf: n ? this.buffer(data) : null };
    if (n) {
      gl.bindVertexArray(this.vao.part);
      gl.bindBuffer(gl.ARRAY_BUFFER, this.quad); gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(0, 0);
      gl.bindBuffer(gl.ARRAY_BUFFER, this.parts.buf);
      const S = 48, at = (loc, size, off) => { gl.enableVertexAttribArray(loc); gl.vertexAttribPointer(loc, size, gl.FLOAT, false, S, off); gl.vertexAttribDivisor(loc, 1); };
      at(1, 4, 0); at(2, 4, 16); at(3, 4, 32);
      gl.bindVertexArray(null);
    }
    this.dirty = true;
  }
  // Highlight a set of point and link indices; everything else dims. null clears.
  highlight(points, links) {
    const gl = this.gl;
    if (!points) { this.dim = 1; } else {
      const ph = new Float32Array(this.pts.n); for (const i of points) if (i < ph.length) ph[i] = 1;
      const lh = new Float32Array(this.lks.n); for (const j of links || []) if (j < lh.length) lh[j] = 1;
      gl.bindBuffer(gl.ARRAY_BUFFER, this.pts.hlBuf); gl.bufferData(gl.ARRAY_BUFFER, ph, gl.DYNAMIC_DRAW);
      gl.bindBuffer(gl.ARRAY_BUFFER, this.lks.hlBuf); gl.bufferData(gl.ARRAY_BUFFER, lh, gl.DYNAMIC_DRAW);
      this.dim = 0.22;
    }
    this.dirty = true;
  }

  // ---- camera ----
  now() { return performance.now() / 1000; }
  animate(seconds) { this.animUntil = Math.max(this.animUntil, this.now() + seconds); this.dirty = true; }
  toScreen(x, y) { return [(x - this.cam.x) * this.cam.z + this.width / 2, (y - this.cam.y) * this.cam.z + this.height / 2]; }
  toWorld(sx, sy) { return [(sx - this.width / 2) / this.cam.z + this.cam.x, (sy - this.height / 2) / this.cam.z + this.cam.y]; }
  get zoom() { return this.cam.z; }
  get moving() { return !!(this.tween || this.spring || this.drag); }
  setCamera(x, y, z) { this.tween = null; this.spring = null; this.cam = { x, y, z }; this.dirty = true; this.emit('camera'); }
  zoomFor(r, pad = 0.1) { return Math.min(this.width, this.height) / (2 * r * (1 + pad)); }
  // Fly to a circle (centre, radius) along a smooth arc: zoom out, travel, zoom in.
  flyTo(x, y, r, ms = 900, pad = 0.12) {
    const z1 = this.zoomFor(r, pad);
    if (this.reduced || ms <= 0) { this.setCamera(x, y, z1); return; }
    const c0 = { ...this.cam }, dist = Math.hypot(x - c0.x, y - c0.y) * Math.min(c0.z, z1);
    const arc = Math.min(0.85, dist / (Math.max(this.width, this.height) * 1.6));
    this.spring = null;
    this.tween = { c0, c1: { x, y, z: z1 }, t0: performance.now(), ms, arc };
    this.dirty = true;
  }
  stopFlight() { if (this.tween) { this.cam = { ...this.tween.c1 }; this.tween = null; this.dirty = true; } }
  step(nowMs) {
    let changed = false;
    if (this.tween) {
      const tw = this.tween, k = Math.min(1, (nowMs - tw.t0) / tw.ms), e = ease(k);
      const lz = Math.log(tw.c0.z) + (Math.log(tw.c1.z) - Math.log(tw.c0.z)) * e;
      this.cam = { x: tw.c0.x + (tw.c1.x - tw.c0.x) * e, y: tw.c0.y + (tw.c1.y - tw.c0.y) * e, z: Math.exp(lz) * (1 - tw.arc * Math.sin(Math.PI * e)) };
      if (k >= 1) { this.cam = { ...tw.c1 }; this.tween = null; }
      changed = true;
    } else if (this.spring) {
      // A critically damped spring per axis (zoom in log space): wheel zoom
      // and inertial pans glide to rest instead of stepping.
      const s = this.spring, dt = Math.min(0.05, (nowMs - s.last) / 1000); s.last = nowMs;
      const w = 18, f = (cur, tgt, v) => { const a = w * w * (tgt - cur) - 2 * w * v; v += a * dt; return [cur + v * dt, v]; };
      let lz = Math.log(this.cam.z), x = this.cam.x, y = this.cam.y;
      [x, s.vx] = f(x, s.x, s.vx); [y, s.vy] = f(y, s.y, s.vy); [lz, s.vz] = f(lz, Math.log(s.z), s.vz);
      this.cam = { x, y, z: Math.exp(lz) };
      const rest = Math.abs(lz - Math.log(s.z)) < 1e-4 && Math.hypot(x - s.x, y - s.y) * this.cam.z < 0.05 && Math.abs(s.vz) < 1e-3 && Math.hypot(s.vx, s.vy) * this.cam.z < 0.5;
      if (rest) { this.cam = { x: s.x, y: s.y, z: s.z }; this.spring = null; }
      changed = true;
    }
    if (changed) { this.dirty = true; this.emit('camera'); }
  }
  springTo(x, y, z) {
    const s = this.spring || { vx: 0, vy: 0, vz: 0 };
    this.tween = null;
    this.spring = { ...s, x, y, z: Math.max(this.minZoom || 1e-4, Math.min(this.maxZoom || 1e4, z)), last: performance.now() };
    this.dirty = true;
  }
  zoomAt(sx, sy, factor) {
    const s = this.spring;
    const base = s ? { x: s.x, y: s.y, z: s.z } : { ...this.cam };
    const z = Math.max(this.minZoom || 1e-4, Math.min(this.maxZoom || 1e4, base.z * factor));
    // Keep the world point under the cursor fixed at the target zoom.
    const wx = (sx - this.width / 2) / base.z + base.x, wy = (sy - this.height / 2) / base.z + base.y;
    this.springTo(wx - (sx - this.width / 2) / z, wy - (sy - this.height / 2) / z, z);
  }

  // ---- input: drag to pan (with inertia), wheel and pinch to zoom, click and hover ----
  on(type, fn) { (this.listeners[type] ||= []).push(fn); }
  emit(type, ...a) { for (const fn of this.listeners[type] || []) fn(...a); }
  bindInput() {
    const c = this.canvas, ptrs = new Map();
    let down = null, moved = false, pinch = null, lastHover = 0;
    const local = (e) => { const r = c.getBoundingClientRect(); return [e.clientX - r.left, e.clientY - r.top]; };
    c.addEventListener('pointerdown', (e) => {
      if (e.button > 0) return;
      try { c.setPointerCapture(e.pointerId); } catch { /* synthetic */ }
      ptrs.set(e.pointerId, local(e));
      this.emit('interact');
      this.stopFlight(); this.spring = null;
      if (ptrs.size === 1) { const p = local(e); down = { p, cam: { ...this.cam }, t: performance.now(), hist: [[performance.now(), p]], slop: e.pointerType === 'mouse' ? 4 : 10 }; moved = false; this.drag = true; }
      if (ptrs.size === 2) { const [a, b] = [...ptrs.values()]; pinch = { d: Math.hypot(a[0] - b[0], a[1] - b[1]), z: this.cam.z, mid: [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2], cam: { ...this.cam } }; moved = true; }
    });
    c.addEventListener('pointermove', (e) => {
      const p = local(e);
      if (ptrs.has(e.pointerId)) ptrs.set(e.pointerId, p);
      if (pinch && ptrs.size === 2) {
        const [a, b] = [...ptrs.values()], d = Math.hypot(a[0] - b[0], a[1] - b[1]), mid = [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
        const z = Math.max(this.minZoom || 1e-4, Math.min(this.maxZoom || 1e4, pinch.z * d / Math.max(pinch.d, 1)));
        const wx = (pinch.mid[0] - this.width / 2) / pinch.cam.z + pinch.cam.x, wy = (pinch.mid[1] - this.height / 2) / pinch.cam.z + pinch.cam.y;
        this.cam = { x: wx - (mid[0] - this.width / 2) / z, y: wy - (mid[1] - this.height / 2) / z, z };
        this.dirty = true; this.emit('camera'); return;
      }
      if (down && ptrs.size === 1) {
        const dx = p[0] - down.p[0], dy = p[1] - down.p[1];
        if (!moved && Math.hypot(dx, dy) > down.slop) moved = true;
        if (moved) {
          this.cam = { x: down.cam.x - dx / down.cam.z, y: down.cam.y - dy / down.cam.z, z: down.cam.z };
          down.hist.push([performance.now(), p]); if (down.hist.length > 6) down.hist.shift();
          this.dirty = true; this.emit('camera');
        }
        return;
      }
      const t = performance.now();
      if (e.pointerType !== 'touch' && t - lastHover > 30) { lastHover = t; this.emit('hover', this.pick(p[0], p[1]), e); }
    });
    const up = (e) => {
      const p = local(e);
      ptrs.delete(e.pointerId);
      if (pinch) {
        // The finger left on the glass pans on from where the pinch ended,
        // never from where it began, and lifting it is never a tap.
        if (ptrs.size < 2) pinch = null;
        if (ptrs.size === 1) { const q = [...ptrs.values()][0]; down = { p: q, cam: { ...this.cam }, t: performance.now(), hist: [[performance.now(), q]], slop: 10 }; moved = true; }
        if (ptrs.size === 0) { down = null; this.drag = false; }
        this.emit('camera'); return;
      }
      if (!down) return;
      this.drag = false;
      if (!moved) { const i = this.pick(p[0], p[1]); this.emit(i === null ? 'background' : 'click', i, e); }
      else if (!this.reduced && down.hist.length > 1) {
        const [t0, p0] = down.hist[0], [t1, p1] = down.hist[down.hist.length - 1], dt = Math.max(16, t1 - t0);
        const vx = (p1[0] - p0[0]) / dt * 1000, vy = (p1[1] - p0[1]) / dt * 1000;
        if (performance.now() - t1 < 80 && Math.hypot(vx, vy) > 60) this.springTo(this.cam.x - vx * 0.22 / this.cam.z, this.cam.y - vy * 0.22 / this.cam.z, this.cam.z);
      }
      down = null; this.emit('camera');
    };
    c.addEventListener('pointerup', up);
    c.addEventListener('pointercancel', up);
    c.addEventListener('pointerleave', () => this.emit('hover', null));
    c.addEventListener('wheel', (e) => {
      e.preventDefault(); this.emit('interact');
      const p = local(e), dy = e.deltaMode === 1 ? e.deltaY * 16 : e.deltaY;
      this.zoomAt(p[0], p[1], Math.exp(-dy * (e.ctrlKey ? 0.01 : 0.0022)));
    }, { passive: false });
    c.addEventListener('dblclick', (e) => { const p = local(e); this.zoomAt(p[0], p[1], 2.4); });
  }

  // The point a tap or click means. Every point is a target at least
  // TARGET_PX across, whatever the zoom. A point whose own disc holds the
  // position wins, the smallest first (the topmost); otherwise the nearest
  // within its target; halos and clouds only when no point is near.
  pick(sx, sy, targetPx = 24) {
    const { n, x, y, r, style } = this.pts;
    if (!n) return null;
    const [wx, wy] = this.toWorld(sx, sy), z = this.cam.z, reach = targetPx / 2 / z;
    let inside = null, insideR = Infinity, near = null, nearD = Infinity, soft = null, softR = Infinity;
    for (let i = 0; i < n; i++) {
      const s = style[i] & 7, halo = s === STYLE.halo || s === STYLE.cloud;
      const rr = r[i], eff = halo ? rr : Math.max(rr, reach);
      const dx = x[i] - wx, dy = y[i] - wy;
      if (Math.abs(dx) > eff || Math.abs(dy) > eff) continue;
      const d = Math.sqrt(dx * dx + dy * dy);
      if (d > eff) continue;
      if (halo) { if (rr < softR) { softR = rr; soft = i; } continue; }
      // Message dots stand for their agent; they never beat a real disc.
      const weight = s === STYLE.msg ? 1.5 : 1;
      if (d <= rr && rr * weight < insideR) { insideR = rr * weight; inside = i; } else if ((d - rr) * weight < nearD) { nearD = (d - rr) * weight; near = i; }
    }
    return inside !== null ? inside : near !== null ? near : soft;
  }
  // Indices of drawn points (not halos) whose centres lie in a screen polygon.
  inPolygon(poly) {
    const out = [], { n, x, y, style } = this.pts;
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const [px, py] of poly) { x0 = Math.min(x0, px); y0 = Math.min(y0, py); x1 = Math.max(x1, px); y1 = Math.max(y1, py); }
    for (let i = 0; i < n; i++) {
      if ((style[i] & 7) === STYLE.halo) continue;
      const [sx, sy] = this.toScreen(x[i], y[i]);
      if (sx < x0 || sx > x1 || sy < y0 || sy > y1) continue;
      let inside = false;
      for (let a = 0, b = poly.length - 1; a < poly.length; b = a++) {
        const [ax, ay] = poly[a], [bx, by] = poly[b];
        if ((ay > sy) !== (by > sy) && sx < (bx - ax) * (sy - ay) / (by - ay) + ax) inside = !inside;
      }
      if (inside) out.push(i);
    }
    return out;
  }

  // ---- a frame: only when something changed or an effect is running ----
  frame(nowMs) {
    this.step(nowMs);
    const t = nowMs / 1000;
    const busy = this.dirty || this.moving || t < this.animUntil || (this.parts.n > 0 && !document.hidden);
    if (!busy) return false;
    this.dirty = false;
    this.resize();
    this.render(t);
    return true;
  }
  render(t) {
    const gl = this.gl, T = this.targets, W = T.scene.w, H = T.scene.h, dark = this.theme.dark ? 1 : 0;
    const motion = this.reduced ? 1000 : 1; // reduced motion: effects finish at once
    gl.bindFramebuffer(gl.FRAMEBUFFER, T.scene.f); gl.viewport(0, 0, W, H);
    gl.clearColor(0, 0, 0, 0); gl.clear(gl.COLOR_BUFFER_BIT);
    gl.enable(gl.BLEND); gl.blendFunc(gl.ONE, gl.ONE_MINUS_SRC_ALPHA);
    const cam = [this.cam.x, this.cam.y, this.cam.z * this.dpr], view = [W, H];
    const common = (pr) => { gl.useProgram(pr.p); gl.uniform3fv(pr.u.cam, cam); gl.uniform2fv(pr.u.view, view); gl.uniform1f(pr.u.time, t); gl.uniform1f(pr.u.dpr, this.dpr); if (pr.u.additive) gl.uniform1f(pr.u.additive, dark); if (pr.u.dim) gl.uniform1f(pr.u.dim, this.dim); if (pr.u.motion) gl.uniform1f(pr.u.motion, motion); };
    if (this.lks.n) { common(this.prog.link); gl.bindVertexArray(this.vao.link); gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, (SEG + 1) * 2, this.lks.n); }
    if (this.parts.n) { common(this.prog.part); gl.bindVertexArray(this.vao.part); gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, this.parts.n); }
    if (this.pts.n) { common(this.prog.point); gl.bindVertexArray(this.vao.point); gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, this.pts.n); }
    gl.disable(gl.BLEND);
    gl.bindVertexArray(this.vao.full);
    const bloom = dark === 1 && !this.noBloom;
    if (bloom) {
      const pass = (pr, dst, src, set) => { gl.bindFramebuffer(gl.FRAMEBUFFER, dst.f); gl.viewport(0, 0, dst.w, dst.h); gl.useProgram(pr.p); gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, src.t); gl.uniform1i(pr.u.src, 0); set(pr); gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4); };
      pass(this.prog.bright, T.a1, T.scene, (pr) => gl.uniform2f(pr.u.texel, 1 / W, 1 / H));
      pass(this.prog.blur, T.b1, T.a1, (pr) => gl.uniform2f(pr.u.dir, 1 / T.a1.w, 0));
      pass(this.prog.blur, T.a1, T.b1, (pr) => gl.uniform2f(pr.u.dir, 0, 1 / T.a1.h));
      pass(this.prog.blur, T.a2, T.a1, (pr) => gl.uniform2f(pr.u.dir, 1.5 / T.a1.w, 0)); // downsample while blurring
      pass(this.prog.blur, T.b2, T.a2, (pr) => gl.uniform2f(pr.u.dir, 0, 1.5 / T.a2.h));
      pass(this.prog.blur, T.a2, T.b2, (pr) => gl.uniform2f(pr.u.dir, 2.5 / T.a2.w, 0));
      pass(this.prog.blur, T.b2, T.a2, (pr) => gl.uniform2f(pr.u.dir, 0, 2.5 / T.a2.h));
    }
    gl.bindFramebuffer(gl.FRAMEBUFFER, null); gl.viewport(0, 0, W, H);
    const pr = this.prog.compose; gl.useProgram(pr.p);
    gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, T.scene.t); gl.uniform1i(pr.u.scene, 0);
    gl.activeTexture(gl.TEXTURE1); gl.bindTexture(gl.TEXTURE_2D, T.a1.t); gl.uniform1i(pr.u.bloom1, 1);
    gl.activeTexture(gl.TEXTURE2); gl.bindTexture(gl.TEXTURE_2D, T.b2.t); gl.uniform1i(pr.u.bloom2, 2);
    gl.uniform3fv(pr.u.bg, this.theme.bg); gl.uniform1f(pr.u.dark, dark); gl.uniform1f(pr.u.bloomOn, bloom ? 1 : 0);
    gl.uniform2fv(pr.u.view, view); gl.uniform3fv(pr.u.cam, cam); gl.uniform1f(pr.u.dpr, this.dpr);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    gl.bindVertexArray(null);
  }
}
