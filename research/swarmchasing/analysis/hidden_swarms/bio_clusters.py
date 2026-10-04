"""Profile-bio clustering: char 3-5-gram TF-IDF over bios of 30+ chars, cosine >= 0.6 between different handles,
union-find; prints clusters of 3+ with boards, creation range and handles (no bio text).
Usage: uv run --with scikit-learn --with numpy python bio_clusters.py
"""
import sys,collections,os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _boards import *
from sklearn.feature_extraction.text import TfidfVectorizer
from sklearn.metrics.pairwise import cosine_similarity
rows=[]
for b in BOARDS:
    P,A=load(b)
    for a in A.values():
        bio=(a.get('bio') or '').strip()
        if len(bio)>=30: rows.append((b,a.get('handle') or a['author_id'][:12],bio,a.get('created_at') or ''))
X=TfidfVectorizer(analyzer='char_wb',ngram_range=(3,5),sublinear_tf=True).fit_transform([r[2] for r in rows])
S=cosine_similarity(X)
n=len(rows); par=list(range(n))
def f(x):
    while par[x]!=x: par[x]=par[par[x]]; x=par[x]
    return x
for i in range(n):
    for j in range(i+1,n):
        if S[i,j]>=0.6 and rows[i][1].lower()!=rows[j][1].lower(): par[f(i)]=f(j)
cl=collections.defaultdict(list)
for i in range(n): cl[f(i)].append(i)
cls=sorted([v for v in cl.values() if len(v)>=3],key=lambda v:-len(v))
print(n,'bios; clusters>=3:',len(cls))
for v in cls[:25]:
    bs=collections.Counter(rows[i][0] for i in v)
    cr=sorted(rows[i][3][:16] for i in v if rows[i][3])
    print(len(v),dict(bs),'created',cr[:1],cr[-1:],[rows[i][1][:18] for i in v][:8])
