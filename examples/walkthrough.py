#!/usr/bin/env python3
"""Bounded production walkthroughs using only the public HTTP service."""
import argparse, concurrent.futures, decimal, json, os, pathlib, shutil, socket, subprocess, tempfile, time, urllib.request, urllib.error
WORKFLOWS=['catalog-create','txn-write','snapshot-read','index-build','query-plan','sql-execute','decimal-exact','txn-conflict','restart','checkpoint','backup-restore','changes','vacuum','transaction-close','statistics']
class Session:
 def __init__(self,binary,root,restore=None):
  self.binary=str(binary);self.root=pathlib.Path(root);self.restore=restore;self.process=None;self.log=None
 def start(self):
  with socket.socket() as sock:
   sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
  self.url=f'http://127.0.0.1:{port}/v1/lamina'
  args=[self.binary,'-data',str(self.root),'-listen',f'127.0.0.1:{port}']
  if self.restore:args+=['-restore',str(self.restore)];self.restore=None
  self.log=tempfile.TemporaryFile();self.process=subprocess.Popen(args,stdout=self.log,stderr=self.log)
  deadline=time.monotonic()+8
  while time.monotonic()<deadline:
   if self.process.poll() is not None:
    self.log.seek(0);raise RuntimeError(self.log.read().decode())
   try:self.call('GET','/health');return self
   except (OSError,urllib.error.URLError):time.sleep(.03)
  raise RuntimeError('service did not become ready')
 def stop(self):
  if self.process and self.process.poll() is None:
   self.process.terminate()
   try:self.process.wait(timeout=12)
   except subprocess.TimeoutExpired:self.process.kill();self.process.wait()
  if self.log:self.log.close();self.log=None
 def call(self,method,path,data=None,status=200,raw=False):
  encoded=None if data is None else json.dumps(data,ensure_ascii=False).encode()
  req=urllib.request.Request(self.url+path,data=encoded,method=method,headers={'Content-Type':'application/json'})
  try:
   with urllib.request.urlopen(req,timeout=12) as response:code=response.status;body=response.read()
  except urllib.error.HTTPError as e:code=e.code;body=e.read()
  if code!=status:raise RuntimeError(f'{method} {path}: expected {status}, got {code}: {body.decode()}')
  return body if raw else json.loads(body)
 def begin(self,readonly=False):return self.call('POST','/transactions',{'read_only':readonly},201)['id']
 def put(self,tx,row,status=200,insert=False):return self.call('POST',f'/transactions/{tx}/put',{'relation':'spectra','row':row,'insert_only':insert},status)
 def commit(self,tx,status=200):return self.call('POST',f'/transactions/{tx}/commit',status=status)
 def query(self,**kwargs):return self.call('POST','/query',{'relation':'spectra',**kwargs})
 def stats(self):return self.call('GET','/stats')
def require(condition,message):
 if not condition:raise RuntimeError(message)
def tuple_(n,band=None,amplitude=None):return {'sample':n,'band':band or f'b{n}','amplitude':n*10 if amplitude is None else amplitude,'valid':True}
def exact(value):
 # Independent oracle: Python's Decimal is exact base-10 arithmetic and never
 # passes through a binary float. This module deliberately runs with -I so no
 # local file shadows the stdlib implementation.
 return decimal.Decimal(value)
def seed(s):
 schema={'name':'spectra','primary':'sample','columns':[{'name':'sample','type':'int'},{'name':'band','type':'text'},{'name':'amplitude','type':'int','nullable':True},{'name':'valid','type':'bool'}]}
 s.call('POST','/relations',schema,201)
 tx=s.begin()
 for n in range(1,7):s.put(tx,tuple_(n))
 s.put(tx,{'sample':7,'band':'b7','amplitude':None,'valid':False})
 return s.commit(tx)['sequence']
def walkthrough(name,binary):
 with tempfile.TemporaryDirectory(prefix='lamina-flow-') as temp:
  temp=pathlib.Path(temp);s=Session(binary,temp/'source').start();other=None
  try:
   seed_seq=seed(s);observations={}
   if name=='catalog-create':
    before=s.stats()['sequence'];bad={'name':'invalid','primary':'missing','columns':[{'name':'value','type':'int'}]}
    s.call('POST','/relations',bad,400);require(s.stats()['sequence']==before,'invalid schema changed sequence')
    cat=s.call('GET','/catalog');require(len(cat['relations'])==1,'catalog mismatch');observations=cat
   elif name=='txn-write':
    tx=s.begin();s.put(tx,tuple_(8));s.put(tx,tuple_(9));require(s.query()['matched']==7,'uncommitted tuples leaked')
    result=s.commit(tx);require(result['writes']==2 and s.query()['matched']==9,'atomic commit failed')
    bad=s.begin();s.put(bad,{'sample':10,'band':'x','amplitude':'bad','valid':True},400)
    s.call('POST',f'/transactions/{bad}/rollback');observations=result
   elif name=='snapshot-read':
    old=s.begin();writer=s.begin();s.put(writer,tuple_(1,amplitude=555));s.commit(writer)
    q={'where':[{'column':'sample','op':'=','value':1}]}
    require(s.query(transaction=old,**q)['rows'][0]['amplitude']==10,'repeatable read violated')
    require(s.query(**q)['rows'][0]['amplitude']==555,'current snapshot stale')
    s.put(old,tuple_(8));require(s.query(transaction=old)['matched']==8,'read-own-write scan missing')
    require(s.query(at=seed_seq)['rows'][0]['amplitude']==10,'time travel failed')
    s.call('POST',f'/transactions/{old}/rollback');s.call('POST','/query',{'relation':'spectra','at':999},410);observations={'old':10,'current':555}
   elif name=='index-build':
    definition={'name':'by_band','relation':'spectra','column':'band','unique':True}
    s.call('POST','/indexes',definition,201)
    a=s.begin();b=s.begin();s.put(a,tuple_(8,band='shared'));s.put(b,tuple_(9,band='shared'));s.commit(a);s.commit(b,409)
    swap=s.begin();s.put(swap,tuple_(1,band='b2'));s.put(swap,tuple_(2,band='b1'));s.commit(swap)
    duplicate=s.begin();s.put(duplicate,tuple_(10,band='b3'));s.commit(duplicate,409)
    observations=s.call('GET','/catalog')
   elif name=='query-plan':
    s.call('POST','/indexes',{'name':'by_amplitude','relation':'spectra','column':'amplitude'},201)
    at=s.stats()['sequence'];tx=s.begin();s.put(tx,tuple_(1,amplitude=999));s.commit(tx)
    q={'where':[{'column':'amplitude','op':'<=','value':20}]}
    fast=s.query(**q);slow=s.query(force_scan=True,**q)
    require(fast['rows']==slow['rows'] and fast['plan']['access']=='index','index/scan mismatch')
    historical=s.query(at=at,**q);require(len(historical['rows'])==2,'historical index lost prior value')
    observations={'indexed':fast,'historical':historical}
   elif name=='sql-execute':
    sql=s.call('POST','/sql',{'sql':'SELECT sample, amplitude FROM spectra WHERE amplitude >= 30 AND valid = true ORDER BY amplitude DESC LIMIT 2;'})
    structured=s.query(columns=['sample','amplitude'],where=[{'column':'amplitude','op':'>=','value':30},{'column':'valid','op':'=','value':True}],order=[{'column':'amplitude','desc':True}],limit=2)
    require(sql['rows']==structured['rows'],'SQL and structured query diverged')
    count=s.call('POST','/sql',{'sql':'SELECT count(amplitude) FROM spectra;'})
    total=s.call('POST','/sql',{'sql':'SELECT sum(amplitude) FROM spectra;'})
    require(count['rows']==[{'count':6}] and total['rows']==[{'sum':210}],'aggregate semantics wrong')
    s.call('POST','/sql',{'sql':'SELECT * FROM spectra; SELECT * FROM spectra;'},400);observations={'projection':sql['rows'],'count':count['rows'],'sum':total['rows']}
   elif name=='decimal-exact':
    schema={'name':'ledger','primary':'id','columns':[{'name':'id','type':'decimal','precision':10,'scale':2},{'name':'amount','type':'decimal','precision':18,'scale':4},{'name':'note','type':'text','nullable':True}]}
    s.call('POST','/relations',schema,201)
    tx=s.begin()
    # The same identity written with different textual spellings; each pair
    # must replace rather than add a row. Values come from quoted strings and
    # raw JSON numbers, and none is ever converted to binary float.
    for row in ({'id':'1.50','amount':'0.1000','note':'a'},{'id':'1.5','amount':'0.2','note':'b'},
                {'id':'2.00','amount':0.3,'note':'c'},{'id':'2.000','amount':'0.4000','note':'d'},
                {'id':'3.000','amount':'0.0005','note':'e'}):
     s.call('POST',f'/transactions/{tx}/put',{'relation':'ledger','row':row})
    s.commit(tx)
    rows=s.call('POST','/query',{'relation':'ledger','order':[{'column':'id'}]})
    require([r['id'] for r in rows['rows']]==['1.50','2.00','3.00'],'decimal identity merged or split rows')
    require(rows['matched']==3,'decimal primary spellings were not one identity')
    # Equality against a differently spelled literal must use the same fact.
    eq=s.call('POST','/query',{'relation':'ledger','where':[{'column':'id','op':'=','value':'2.000000'}]})
    require(eq['matched']==1 and eq['rows'][0]['note']=='d','decimal equality crossed spellings')
    # Rounding is declared per column; reject is the default and never guesses.
    s.call('POST','/relations',{'name':'rounding','primary':'k','columns':[{'name':'k','type':'int'},{'name':'rej','type':'decimal','precision':6,'scale':2,'nullable':True},{'name':'hev','type':'decimal','precision':6,'scale':2,'rounding':'half_even','nullable':True},{'name':'hup','type':'decimal','precision':6,'scale':2,'rounding':'half_up','nullable':True},{'name':'trc','type':'decimal','precision':6,'scale':2,'rounding':'truncate','nullable':True}]},201)
    tx=s.begin()
    s.call('POST',f'/transactions/{tx}/put',{'relation':'rounding','row':{'k':1,'rej':'1.235'}},400)
    s.call('POST',f'/transactions/{tx}/put',{'relation':'rounding','row':{'k':1,'rej':'1.230','hev':'1.225','hup':'1.225','trc':'1.229'}})
    s.call('POST',f'/transactions/{tx}/put',{'relation':'rounding','row':{'k':2,'rej':'1.000','hev':'1.235','hup':'1.235','trc':'-1.239'}})
    s.commit(tx)
    rounded=s.call('POST','/query',{'relation':'rounding','order':[{'column':'k'}]})['rows']
    require([(r['hev'],r['hup'],r['trc']) for r in rounded]==[('1.22','1.23','1.22'),('1.24','1.24','-1.23')],'declared rounding not applied')
    # A rounded carry that leaves the declared precision is an overflow, not a wider value.
    s.call('POST','/relations',{'name':'narrow','primary':'k','columns':[{'name':'k','type':'int'},{'name':'v','type':'decimal','precision':3,'scale':2,'rounding':'half_up','nullable':True}]},201)
    tx=s.begin();s.call('POST',f'/transactions/{tx}/put',{'relation':'narrow','row':{'k':1,'v':'9.999'}},400);s.call('POST',f'/transactions/{tx}/rollback')
    # A decimal primary key owns its own exact identity; a rounding column that
    # follows it must still apply its own declared policy rather than inherit
    # the primary's exactness.
    s.call('POST','/relations',{'name':'hybrid','primary':'k','columns':[{'name':'k','type':'decimal','precision':6,'scale':2},{'name':'v','type':'decimal','precision':6,'scale':2,'rounding':'half_up','nullable':True}]},201)
    tx=s.begin();s.call('POST',f'/transactions/{tx}/put',{'relation':'hybrid','row':{'k':'1.00','v':'1.235'}});s.commit(tx)
    hybrid=s.call('POST','/query',{'relation':'hybrid'})['rows']
    require(hybrid==[{'k':'1.00','v':'1.24'}],'column after decimal primary lost its rounding policy')
    # Aggregation is exact and agrees across SQL, structured query and an
    # independent decimal oracle. Decimal sums render at the column scale.
    values=[exact(r['amount']) for r in rows['rows']]
    expected=format(sum(values,exact(0)),'f')
    structured=s.call('POST','/query',{'relation':'ledger','aggregate':{'function':'sum','column':'amount'}})
    sql=s.call('POST','/sql',{'sql':'SELECT sum(amount) FROM ledger;'})
    require(structured['rows']==sql['rows'],'SQL and structured decimal sum diverged')
    require(exact(structured['rows'][0]['sum'])==sum(values,exact(0)),'decimal sum lost precision')
    require(structured['plan']==sql['plan'],'aggregate plan facts diverged')
    # An exact index candidate set must match a forced scan over the same snapshot.
    s.call('POST','/indexes',{'name':'by_amount','relation':'ledger','column':'amount'},201)
    where=[{'column':'amount','op':'>=','value':'0.2000'}]
    fast=s.call('POST','/query',{'relation':'ledger','where':where,'order':[{'column':'id'}]})
    slow=s.call('POST','/query',{'relation':'ledger','where':where,'order':[{'column':'id'}],'force_scan':True})
    require(fast['rows']==slow['rows'] and fast['plan']['access']=='index','decimal index/scan mismatch')
    # Recovery and the portable backup must preserve the same facts.
    before=s.call('POST','/query',{'relation':'ledger','order':[{'column':'id'}]})['rows']
    s.call('POST','/checkpoint');s.stop();s.start()
    require(s.call('POST','/query',{'relation':'ledger','order':[{'column':'id'}]})['rows']==before,'restart changed decimal facts')
    recovered=s.call('POST','/query',{'relation':'ledger','aggregate':{'function':'sum','column':'amount'}})
    require(exact(recovered['rows'][0]['sum'])==sum(values,exact(0)),'recovered decimal sum changed')
    backup=s.call('GET','/backup',raw=True);path=temp/'ledger.lmi';path.write_bytes(backup)
    other=Session(binary,temp/'decimal-restored',path).start()
    require(other.call('POST','/query',{'relation':'ledger','order':[{'column':'id'}]})['rows']==before,'restored decimal facts changed')
    # A decimal primary key may not declare a rounding policy: identity must
    # not depend on which spelling happened to arrive first.
    s.call('POST','/relations',{'name':'bad','primary':'id','columns':[{'name':'id','type':'decimal','precision':5,'scale':2,'rounding':'half_up'}]},400)
    observations={'rows':before,'structured_sum':structured['rows'][0]['sum'],'sql_sum':sql['rows'][0]['sum'],'oracle_sum':expected,'rounded':rounded}
   elif name=='txn-conflict':
    a=s.begin();b=s.begin();s.put(a,tuple_(1,amplitude=101));s.put(b,tuple_(1,amplitude=202));s.put(b,tuple_(8))
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
     jobs=[pool.submit(s.commit,a),pool.submit(s.commit,b)]
     results=[]
     for job in jobs:
      try:results.append({'commit':job.result()})
      except RuntimeError as e:
       require('got 409' in str(e),'unexpected concurrent failure');results.append({'conflict':True})
    require(sum('conflict' in r for r in results)==1,'exactly one writer must lose')
    rows=s.query()['rows'];one=next(r for r in rows if r['sample']==1)
    require((one['amplitude']==202)==any(r['sample']==8 for r in rows),'conflicting commit partially published')
    s.commit(a,404);s.commit(b,404);observations={'results':results,'rows':rows}
   elif name=='restart':
    before=s.query()['rows'];s.stop()
    with (s.root/'journal.lmw').open('ab') as f:f.write(b'\x00\x00\x00')
    s.start();require(s.query()['rows']==before,'tail recovery changed committed data');require(s.stats()['recovered_tail_bytes']==3,'tail recovery not reported')
    s.stop();broken=temp/'broken';shutil.copytree(s.root,broken)
    data=bytearray((broken/'journal.lmw').read_bytes());data[-1]^=1;(broken/'journal.lmw').write_bytes(data)
    result=subprocess.run([str(binary),'-data',str(broken),'-listen','127.0.0.1:0'],capture_output=True,text=True,timeout=5)
    require(result.returncode!=0 and 'checksum' in result.stderr,'full frame corruption accepted')
    observations={'tail_bytes':3,'corruption':'rejected'}
   elif name=='checkpoint':
    at=s.call('POST','/checkpoint')['sequence'];tx=s.begin();s.put(tx,tuple_(8));s.commit(tx)
    before=s.query()['rows'];s.stop();s.start();require(s.query()['rows']==before,'checkpoint plus suffix recovery failed')
    locked=subprocess.run([str(binary),'-data',str(s.root),'-listen','127.0.0.1:0'],capture_output=True,text=True,timeout=5)
    require(locked.returncode!=0 and 'already open' in locked.stderr,'exclusive directory ownership missing');observations={'checkpoint':at,'head':s.stats()['sequence']}
   elif name=='backup-restore':
    backup=s.call('GET','/backup',raw=True);path=temp/'portable.lmi';path.write_bytes(backup)
    frozen=s.query()['rows'];tx=s.begin();s.put(tx,tuple_(8));s.commit(tx)
    other=Session(binary,temp/'restored',path).start();require(other.query()['rows']==frozen,'backup mutated with source')
    tx=other.begin();other.put(tx,tuple_(9));result=other.commit(tx);other.stop();other.start()
    require(other.query()['matched']==8,'restored journal base does not continue')
    other.call('GET','/changes?after=0',status=410)
    broken=json.loads(backup);broken['sha256']='0'*64;bad=temp/'bad.lmi';bad.write_text(json.dumps(broken))
    failed=subprocess.run([str(binary),'-restore',str(bad),'-data',str(temp/'badrestore')],capture_output=True,text=True,timeout=5)
    require(failed.returncode!=0 and not (temp/'badrestore').exists(),'invalid backup published');observations={'restored_commit':result,'original_sequence':seed_seq}
   elif name=='changes':
    tx=s.begin()
    for n in range(8,12):s.put(tx,tuple_(n))
    committed=s.commit(tx);page=s.call('GET',f'/changes?after={seed_seq}&limit=1')
    require(len(page['records'])==1 and len(page['records'][0]['writes'])==4 and page['cursor']==committed['sequence'],'change page split transaction')
    s.call('GET','/changes?after=999',status=400);observations=page
   elif name=='vacuum':
    old=s.begin();tx=s.begin();s.put(tx,tuple_(1,amplitude=99));s.commit(tx);requested=s.stats()['sequence']
    pinned=s.call('POST','/vacuum',{'horizon':requested});require(pinned['pinned'] and pinned['horizon']==seed_seq,'vacuum crossed active snapshot')
    require(s.query(transaction=old)['rows'][0]['amplitude']==10,'active snapshot lost')
    s.call('POST',f'/transactions/{old}/rollback');reclaimed=s.call('POST','/vacuum',{'horizon':requested})
    require(reclaimed['removed']>=1,'obsolete version not reclaimed');s.call('POST','/query',{'relation':'spectra','at':seed_seq},410)
    s.call('POST','/checkpoint');s.stop();s.start();require(s.query()['rows'][0]['amplitude']==99,'vacuum did not survive restart');observations={'pinned':pinned,'reclaimed':reclaimed}
   elif name=='transaction-close':
    tx=s.begin();s.put(tx,tuple_(8));s.call('POST',f'/transactions/{tx}/rollback');s.commit(tx,404)
    require(s.query()['matched']==7 and not s.stats()['active'],'rollback leaked state or pin')
    tx=s.begin(True);s.put(tx,tuple_(9),409);s.call('POST',f'/transactions/{tx}/rollback');observations={'rollback':'discarded','read_only':'enforced'}
   elif name=='statistics':
    s.call('POST','/indexes',{'name':'by_amp','relation':'spectra','column':'amplitude'},201)
    q=s.query(where=[{'column':'amplitude','op':'>=','value':40}],limit=2)
    require(q['matched']==3 and q['scanned']==3 and len(q['rows'])==2 and q['truncated'],'query fact meanings conflated')
    observations={'query':q,'stats':s.stats()}
   else:raise ValueError(name)
   return {'workflow':name,'status':'observed','observations':observations}
  finally:
   s.stop()
   if other:other.stop()
def main():
 parser=argparse.ArgumentParser();parser.add_argument('--workflow',choices=['all',*WORKFLOWS],default='all');args=parser.parse_args()
 project=pathlib.Path(__file__).resolve().parents[1];binary=project/'bin/laminad'
 if not binary.exists():raise SystemExit('build first: go build -o bin/laminad ./cmd/laminad')
 names=WORKFLOWS if args.workflow=='all' else [args.workflow]
 for name in names:print(json.dumps(walkthrough(name,binary),ensure_ascii=False),flush=True)
if __name__=='__main__':main()
