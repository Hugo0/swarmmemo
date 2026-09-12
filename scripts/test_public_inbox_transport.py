"""Fresh-process public worker isolation and cancellation; synthetic/loopback only."""
import os
from pathlib import Path
import subprocess
import sys
import unittest

CLIENT_DIR = Path(__file__).resolve().parents[1] / "clients/python"

COMMON = r'''
import ctypes, inspect, json, os, select, signal, subprocess, sys, tempfile, threading, time
from pathlib import Path
from unittest.mock import patch
sys.path.insert(0, sys.argv[1])
import swarmmemo_inbox as p

def binding():
    return {'version':1,'origin':'http://127.0.0.1:9','service_id':'swarmmemo.com',
            'recipient':'a'*64,'room':'','visibility':'public','reader_public_key':'','start_mode':'history'}
reader=p.Inbox(Path('/tmp/unused-public-transport-fixture'),binding())
real_spawn=subprocess.Popen
spawned=[]
worker_code="import sys; sys.stdin.buffer.read(); print('{\"status\":200,\"body\":\"e30=\"}')"
def spawn(args,**kwargs):
    child=real_spawn([args[0],'-I','-B','-c',worker_code],**kwargs)
    spawned.append(child)
    return child
def stop(child):
    if child.poll() is None: child.kill()
    child.wait(timeout=2)
    for stream in (child.stdin,child.stdout):
        if stream is not None: stream.close()
def assert_reaped():
    assert spawned
    for child in spawned:
        assert child.poll() is not None
        assert child.stdout.closed
def fetch(seconds=2):
    return reader._fetch('/api/changes?after=-1',time.monotonic()+seconds)
'''


class PublicTransportTests(unittest.TestCase):
    def run_case(self, code, timeout=12):
        environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
                       "PYTHONDONTWRITEBYTECODE": "1"}
        result = subprocess.run([sys.executable, "-I", "-B", "-W", "error::ResourceWarning", "-c", COMMON + code, str(CLIENT_DIR)],
                                env=environment, capture_output=True, timeout=timeout)
        self.assertEqual(result.returncode, 0, (result.stdout + result.stderr).decode("utf-8", "replace"))
        self.assertEqual(result.stderr, b"")

    def test_real_spawn_sigint_before_assignment_is_reaped(self):
        self.run_case(r'''
def interrupt(args,**kwargs):
    child=spawn(args,**kwargs)
    os.kill(os.getpid(),signal.SIGINT)
    return child
try:
    with patch.object(p.subprocess,'Popen',interrupt):
        try: fetch()
        except KeyboardInterrupt: pass
        else: raise AssertionError('caller SIGINT was swallowed')
    assert_reaped()
finally:
    for child in spawned: stop(child)
''')

    def test_real_second_sigint_at_cleanup_entry_and_repeated_kill_signals(self):
        self.run_case(r'''
worker_code='import time; time.sleep(20)'
cleanup_line=inspect.getsourcelines(p.Inbox._fetch_owned)[1]
lines=inspect.getsourcelines(p.Inbox._fetch_owned)[0]
cleanup_line+=next(i for i,line in enumerate(lines) if line.strip()=='if worker is not None:')
injected=[]
def trace(frame,event,arg):
    if frame.f_code is p.Inbox._fetch_owned.__code__ and event=='line' and frame.f_lineno==cleanup_line and not injected:
        injected.append(True)
        os.kill(os.getpid(),signal.SIGINT)
    return trace
def instrument(args,**kwargs):
    child=spawn(args,**kwargs)
    kill=child.kill
    def repeated():
        os.kill(os.getpid(),signal.SIGINT)
        os.kill(os.getpid(),signal.SIGINT)
        return kill()
    child.kill=repeated
    os.kill(os.getpid(),signal.SIGINT)
    return child
try:
    sys.settrace(trace)
    with patch.object(p.subprocess,'Popen',instrument):
        try: fetch()
        except KeyboardInterrupt: pass
        else: raise AssertionError('caller SIGINT was swallowed')
    sys.settrace(None)
    assert injected==[True]
    assert_reaped()
finally:
    sys.settrace(None)
    for child in spawned: stop(child)
''')

    def test_custom_ignored_and_blocked_signal_semantics(self):
        self.run_case(r'''
originals={n:signal.getsignal(n) for n in (signal.SIGINT,signal.SIGTERM,signal.SIGHUP)}
original_mask=signal.pthread_sigmask(signal.SIG_BLOCK,[])
delivered=[]
def handler(number,frame):
    assert_reaped()
    delivered.append(number)
def instrument(args,**kwargs):
    child=spawn(args,**kwargs)
    signal.pthread_kill(threading.get_ident(),signal.SIGTERM)
    signal.pthread_kill(threading.get_ident(),signal.SIGHUP)
    return child
try:
    for n in (signal.SIGTERM,signal.SIGHUP): signal.signal(n,handler)
    with patch.object(p.subprocess,'Popen',instrument):
        try: fetch()
        except p.InboxError as e: assert str(e)=='interrupted'
        else: raise AssertionError('custom signal did not cancel')
    assert set(delivered)=={signal.SIGTERM,signal.SIGHUP}
    for n in (signal.SIGTERM,signal.SIGHUP): assert signal.getsignal(n) is handler
    delivered.clear()
    for n in (signal.SIGTERM,signal.SIGHUP): signal.signal(n,signal.SIG_IGN)
    with patch.object(p.subprocess,'Popen',instrument): assert fetch()==(200,{})
    assert delivered==[]
    for n in (signal.SIGTERM,signal.SIGHUP): signal.signal(n,handler)
    signal.pthread_sigmask(signal.SIG_BLOCK,[signal.SIGTERM,signal.SIGHUP])
    with patch.object(p.subprocess,'Popen',instrument): assert fetch()==(200,{})
    assert delivered==[]
    assert {signal.SIGTERM,signal.SIGHUP} <= signal.pthread_sigmask(signal.SIG_BLOCK,[])
    signal.pthread_sigmask(signal.SIG_SETMASK,original_mask)
    assert set(delivered)=={signal.SIGTERM,signal.SIGHUP}
    assert_reaped()
finally:
    for n,h in originals.items(): signal.signal(n,h)
    signal.pthread_sigmask(signal.SIG_SETMASK,original_mask)
    for child in spawned: stop(child)
''')

    def test_signal_during_restoration_cannot_skip_other_original_handlers(self):
        self.run_case(r'''
numbers=(signal.SIGINT,signal.SIGTERM,signal.SIGHUP)
originals={n:signal.getsignal(n) for n in numbers}
original_mask=signal.pthread_sigmask(signal.SIG_BLOCK,[])
ready,finish=threading.Event(),threading.Event()
def secondary():
    signal.pthread_sigmask(signal.SIG_UNBLOCK,[signal.SIGINT])
    ready.set(); finish.wait(5)
thread=threading.Thread(target=secondary);thread.start();assert ready.wait(2)
real_signal=signal.signal
injected=[]
def custom_term(n,f): pass
def instrument(n,h):
    result=real_signal(n,h)
    if n==signal.SIGINT and h is originals[n] and not injected:
        injected.append(True)
        signal.pthread_kill(thread.ident,signal.SIGINT)
        time.sleep(.02)
    return result
try:
    real_signal(signal.SIGTERM,custom_term);real_signal(signal.SIGHUP,signal.SIG_IGN)
    expected={n:signal.getsignal(n) for n in numbers}
    with patch.object(p.signal,'signal',instrument),patch.object(p.subprocess,'Popen',spawn):
        try: fetch()
        except KeyboardInterrupt: pass
        else: raise AssertionError('restoration signal not delivered')
    assert injected==[True]
    assert {n:signal.getsignal(n) for n in numbers}==expected
    assert signal.pthread_sigmask(signal.SIG_BLOCK,[])==original_mask
    assert_reaped()
finally:
    finish.set();thread.join(2)
    for n,h in originals.items(): real_signal(n,h)
    signal.pthread_sigmask(signal.SIG_SETMASK,original_mask)
    for child in spawned: stop(child)
''')

    def test_oversized_stdout_is_rejected_during_collection(self):
        self.run_case(r'''
worker_code="import os,time; block=b'x'*65536\nwhile True: os.write(1,block)"
reads=[]
real_read=os.read
def bounded(fd,size):
    chunk=real_read(fd,size)
    if spawned and fd==spawned[0].stdout.fileno(): reads.append(len(chunk))
    return chunk
try:
    start=time.monotonic()
    with patch.object(p.subprocess,'Popen',spawn),patch.object(p.os,'read',bounded):
        try: fetch(2)
        except p.InboxError as e: assert str(e)=='response_byte_limit'
        else: raise AssertionError('accepted oversized IPC')
    assert sum(reads)<=p.MAX_IPC+1
    assert time.monotonic()-start<2
    assert_reaped()
finally:
    for child in spawned: stop(child)
''')

    def test_descendant_held_pipe_does_not_block_direct_child_cleanup(self):
        self.run_case(r'''
assert ctypes.CDLL(None).prctl(36,1,0,0,0)==0
with tempfile.TemporaryDirectory() as root:
    marker=Path(root)/'descendant'
    worker_code="import os,time,pathlib; child=os.fork(); pathlib.Path("+repr(str(marker))+ ").write_text(str(os.getpid())) if child==0 else None; time.sleep(20) if child==0 else time.sleep(20)"
    descendant=None
    try:
        start=time.monotonic()
        with patch.object(p.subprocess,'Popen',spawn):
            try: fetch(.8)
            except p.InboxError as e: assert str(e)=='deadline_exceeded'
            else: raise AssertionError('held pipe did not time out')
        assert time.monotonic()-start<1.5
        assert_reaped()
        descendant=int(marker.read_text())
        # The contract intentionally does not kill descendants. Now adopted by
        # this fixture's subreaper, it remains our own unreaped child to clean up.
        assert os.waitpid(descendant,os.WNOHANG)==(0,0)
    finally:
        for child in spawned: stop(child)
        if descendant is None and marker.exists(): descendant=int(marker.read_text())
        if descendant is not None:
            got,_=os.waitpid(descendant,os.WNOHANG)
            if not got: os.kill(descendant,signal.SIGKILL);os.waitpid(descendant,0)
''')

    def test_cleanup_failure_is_bounded_and_not_success(self):
        self.run_case(r'''
def instrument(args,**kwargs):
    child=spawn(args,**kwargs)
    original_wait=child.wait
    def fail_wait(timeout=None):
        assert timeout is not None and 0<=timeout<=2
        raise subprocess.TimeoutExpired('fixed-fixture',timeout)
    child.wait=fail_wait
    child.original_wait=original_wait
    return child
try:
    with patch.object(p.subprocess,'Popen',instrument):
        try: fetch()
        except p.InboxError as e: assert str(e)=='worker_cleanup_failed'
        else: raise AssertionError('reported success after failed cleanup')
    assert spawned[0].stdout.closed
finally:
    for child in spawned:
        child.wait=child.original_wait
        stop(child)
''')

    def test_platform_thread_and_sigchld_refuse_before_resync_mutation(self):
        self.run_case(r'''
with tempfile.TemporaryDirectory() as root:
    reader=p.Inbox(Path(root)/'inbox.sqlite',binding());reader.create();reader.add_consumer('reader')
    before=reader.status()
    failures=[]
    def attempt():
        assert reader.status()==before
        assert reader.pending('reader')==[]
        for action in (reader.poll,reader.resync,fetch):
            try: action()
            except p.InboxError as e: failures.append(str(e))
            else: raise AssertionError('unsupported caller accepted')
    with patch.object(p.subprocess,'Popen',side_effect=AssertionError('must not spawn')):
        thread=threading.Thread(target=attempt);thread.start();thread.join(2);assert not thread.is_alive()
        assert failures==['worker_main_thread_required']*3
        for handler in (signal.SIG_IGN,lambda n,f:None):
            original=signal.signal(signal.SIGCHLD,handler)
            try: attempt()
            finally: signal.signal(signal.SIGCHLD,original)
        with patch.object(p.sys,'platform','unsupported'): attempt()
    assert failures[3:]==['worker_platform_required']*9
    assert reader.status()==before
''')

    def test_filtered_environment_isolation_and_actual_keyless_get(self):
        self.run_case(r'''
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
import importlib.util,shutil
requests=[]
class Handler(BaseHTTPRequestHandler):
    def log_message(self,*a): pass
    def do_GET(self):
        requests.append((self.path,dict(self.headers)))
        self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length','2');self.end_headers();self.wfile.write(b'{}')
server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
try:
    with tempfile.TemporaryDirectory() as root:
        directory=Path(root)
        for name in ('swarmmemo.py','swarmmemo_inbox.py'): shutil.copy2(Path(p.__file__).parent/name,directory/name)
        (directory/'sitecustomize.py').write_text("raise RuntimeError('POISON_STARTUP_CANARY')")
        spec=importlib.util.spec_from_file_location('isolated_inbox_fixture',directory/'swarmmemo_inbox.py')
        fixture=importlib.util.module_from_spec(spec);spec.loader.exec_module(fixture)
        config=binding();config['origin']='http://127.0.0.1:'+str(server.server_port)
        reader=fixture.Inbox(directory/'unused.sqlite',config)
        poison={'PYTHONPATH':str(directory),'PYTHONHOME':'/missing-python-home','HTTP_PROXY':'http://127.0.0.1:1',
                'HTTPS_PROXY':'http://127.0.0.1:1','AWS_SECRET_ACCESS_KEY':'SYNTHETIC_SECRET',
                'SSL_CERT_FILE':'/missing-ca','SWARMMEMO_KEY':'/never-read-key'}
        def inspect_spawn(args,**kwargs):
            assert args[1:3]==['-I','-B']
            assert set(kwargs['env'])=={'PATH','LANG','LC_ALL'}
            assert kwargs['close_fds'] is True and kwargs['start_new_session'] is True
            assert all(key not in kwargs['env'] for key in poison)
            child=real_spawn(args,**kwargs);spawned.append(child);return child
        with patch.dict(os.environ,poison),patch.object(p.subprocess,'Popen',inspect_spawn),patch.object(p.memo,'load_key',side_effect=AssertionError('no keys')):
            assert reader._fetch('/api/changes?after=-1',time.monotonic()+2)==(200,{})
        assert len(requests)==1 and requests[0][0]=='/api/changes?after=-1'
        assert not any(key.lower() in ('authorization','cookie') for key in requests[0][1])
        assert not list(directory.rglob('__pycache__'))
        assert not (directory/'unused.sqlite').exists()
        assert_reaped()
finally:
    server.shutdown();server.server_close();thread.join(2)
    for child in spawned: stop(child)
''')

    def test_worker_refuses_missing_or_incorrect_parent_before_input(self):
        self.run_case(r'''
for tail in (['_fetch'],['_fetch','1'],['_fetch',str(os.getpid()+10000000)]):
    child=real_spawn([sys.executable,'-I','-B',p.__file__,*tail],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    try:
        child.wait(timeout=2) # stdin still open: refusal precedes reading it.
        assert child.returncode!=0
        assert child.stdout.read()==b'' and child.stderr.read()==b''
    finally:
        child.stdin.close();child.stdout.close();child.stderr.close()
''')

    def test_parent_death_before_arm_and_during_dns_stall(self):
        self.run_case(r'''
assert ctypes.CDLL(None).prctl(36,1,0,0,0)==0
for mode in ('before-arm','dns'):
    with tempfile.TemporaryDirectory() as root:
        marker=Path(root)/'dns-ready'
        wrapper_code="import os,runpy,socket,sys,time,pathlib\n"+(
            "time.sleep(.4)\n" if mode=='before-arm' else
            "marker=sys.argv[3]\ndef stalled(*a,**k):\n pathlib.Path(marker).write_text('ready')\n time.sleep(20)\nsocket.getaddrinfo=stalled\n")+"sys.argv=[sys.argv[1],'_fetch',sys.argv[2]];runpy.run_path(sys.argv[0],run_name='__main__')"
        parent_code="import os,subprocess,sys,time;sys.path.insert(0,sys.argv[1]);import swarmmemo_inbox as p\nreal=subprocess.Popen\ndef launch(args,**kwargs):\n child=real([args[0],'-I','-B','-c',sys.argv[3],args[3],args[5],sys.argv[4]],**kwargs)\n print(child.pid,flush=True)\n return child\np.subprocess.Popen=launch\np.Inbox('/tmp/unused-parent-death-fixture',p.strict_json(sys.argv[2]))._fetch('/api/changes?after=-1',time.monotonic()+10)"
        parent=real_spawn([sys.executable,'-I','-B','-c',parent_code,str(Path(p.__file__).parent),json.dumps(binding()),wrapper_code,str(marker)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
        pid=None
        try:
            assert select.select([parent.stdout],[],[],3)[0]
            pid=int(parent.stdout.readline())
            if mode=='dns':
                until=time.monotonic()+3
                while not marker.exists() and time.monotonic()<until: time.sleep(.01)
                assert marker.exists()
            parent.kill();parent.wait(timeout=2)
            until=time.monotonic()+2
            while time.monotonic()<until:
                got,_=os.waitpid(pid,os.WNOHANG)
                if got: pid=None;break
                time.sleep(.01)
            assert pid is None,'fetch worker survived parent death'
        finally:
            if parent.poll() is None: parent.kill();parent.wait(timeout=2)
            parent.stdout.close()
            if pid is not None:
                try: got,_=os.waitpid(pid,os.WNOHANG)
                except ChildProcessError: pass # Its live caller already reaped it.
                else:
                    if not got: os.kill(pid,signal.SIGKILL);os.waitpid(pid,0)
''')


if __name__ == "__main__": unittest.main()
