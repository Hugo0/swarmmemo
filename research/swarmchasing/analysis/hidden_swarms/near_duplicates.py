"""Near-duplicate posts across authors: char 4-5-gram TF-IDF, cosine >= TH (default 0.8), union-find into clusters.
Writes $SWARMCHASING_OUT/near_duplicates_<TH>.json with (board, author_id, name, time, post_id) per member; no text.
Usage: uv run --with scikit-learn --with numpy --with scipy python near_duplicates.py [TH]
"""
import sys,collections,json,re,os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _boards import *
import numpy as np
from sklearn.feature_extraction.text import TfidfVectorizer
from sklearn.neighbors import NearestNeighbors
docs=[];meta=[]
for b in BOARDS:
    P,A=load(b)
    for p in P:
        t=(p['text'] or '').strip()
        if len(t)<80: continue
        docs.append(t[:3000]); meta.append((b,p['author_id'],name(A,p['author_id']),p['t'],p['post_id']))
print(len(docs))
X=TfidfVectorizer(analyzer='char_wb',ngram_range=(4,5),min_df=2,max_features=300000,sublinear_tf=True).fit_transform(docs)
nn=NearestNeighbors(n_neighbors=11,metric='cosine').fit(X)
D,I=nn.kneighbors(X)
TH=float(sys.argv[1]) if len(sys.argv)>1 else 0.8
# union-find over cross-author pairs with sim>=TH
par=list(range(len(docs)))
def f(x):
    while par[x]!=x: par[x]=par[par[x]]; x=par[x]
    return x
edges=0; crossb=0
for i in range(len(docs)):
    for d,j in zip(D[i][1:],I[i][1:]):
        s=1-d
        if s>=TH and meta[i][1]!=meta[j][1] and not (meta[i][1] in ('anonymous','name:Anonymous') and meta[j][1] in ('anonymous','name:Anonymous') and False):
            par[f(i)]=f(j); edges+=1
            if meta[i][0]!=meta[j][0]: crossb+=1
cl=collections.defaultdict(list)
for i in range(len(docs)): cl[f(i)].append(i)
cls=[v for v in cl.values() if len(set(meta[i][1] for i in v))>=2]
cls.sort(key=lambda v:-len(set(meta[i][1] for i in v)))
print('TH',TH,'cross-author edges',edges,'cross-board edges',crossb,'clusters',len(cls))
out=[]
for v in cls[:40]:
    au=collections.Counter((meta[i][0],meta[i][2]) for i in v)
    bs=collections.Counter(meta[i][0] for i in v)
    ts_=sorted(meta[i][3] for i in v)
    print(len(v),'posts',len(au),'authors',dict(bs),f"span {(ts_[-1]-ts_[0])/3600:.1f}h",list(au)[:6])
    out.append([meta[i] for i in v])
json.dump(out,open(out_path(f'near_duplicates_{TH}.json'),'w'))
