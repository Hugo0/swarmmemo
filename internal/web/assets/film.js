// The launch film (templates/page.html "film"). Without this script the video shows its poster
// and native controls. With it: muted autoplay and loop, unless the reader prefers reduced
// motion (then the poster waits under a play button); a sound button that starts the film over
// with sound the first time; the square poster on a phone; and no playback while off screen.
(() => {
  const still = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  for (const fig of document.querySelectorAll('[data-film]')) {
    const video = fig.querySelector('video'), play = fig.querySelector('.film-play'), sound = fig.querySelector('.film-sound');
    if (!video || !play || !sound) continue;
    if (window.matchMedia('(max-width: 600px)').matches && video.dataset.posterSquare) video.poster = video.dataset.posterSquare;
    video.controls = false;
    let wanted = !still, heard = false;
    const label = () => {
      sound.textContent = video.muted ? 'Sound on' : 'Sound off';
      sound.setAttribute('aria-pressed', String(!video.muted));
    };
    const start = () => video.play().then(() => { play.hidden = true; sound.hidden = false; }, () => { play.hidden = false; });
    play.addEventListener('click', () => {
      wanted = true; video.muted = false; heard = true; video.currentTime = 0; label(); start();
    });
    sound.addEventListener('click', () => {
      video.muted = !video.muted;
      if (!video.muted && !heard) { heard = true; video.currentTime = 0; }
      label();
      if (video.paused) { wanted = true; start(); }
    });
    video.addEventListener('click', () => { if (!video.paused) { video.pause(); wanted = false; play.hidden = false; } });
    label();
    if (still) { play.hidden = false; } else { video.muted = true; start(); }
    if ('IntersectionObserver' in window) {
      new IntersectionObserver(([e]) => {
        if (!e.isIntersecting && !video.paused) video.pause();
        else if (e.isIntersecting && wanted && video.paused) start();
      }, { threshold: 0.25 }).observe(video);
    }
  }
})();
