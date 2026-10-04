"""KMeans (K, default 30; the paper uses K=28) over the addition embeddings, c-TF-IDF top terms per cluster,
per-phase counts and a few short samples for hand labelling. Writes cache/clusters_k<K>.json (holds samples: local only).
Usage: K=28 uv run --with scikit-learn --with numpy python cluster_additions.py
"""
import os,json,re,collections,random
from _posts import cache, load_additions
os.environ['OMP_NUM_THREADS']='8'
import numpy as np
from sklearn.cluster import KMeans
from sklearn.feature_extraction.text import TfidfVectorizer
E=np.load(cache('E.npy')); uniq=json.load(open(cache('uniq.json')))
A=load_additions()
def norm(s):
    s=re.sub(r'https?://([A-Za-z0-9.-]+)\S*',lambda m:' URL_'+m.group(1).replace('.','_')+' ',s)
    s=re.sub(r'\b\d{6,}\b',' NUM ',s)
    return re.sub(r'\s+',' ',s).strip()[:500]
idx={t:i for i,t in enumerate(uniq)}
K=int(os.environ.get('K','30'))
km=KMeans(K,n_init=4,random_state=0).fit(E); lab=km.labels_
np.save(cache('lab.npy'),lab)
# c-TF-IDF
docs=collections.defaultdict(list)
for t,l in zip(uniq,lab): docs[l].append(t)
tv=TfidfVectorizer(token_pattern=r'[A-Za-z_][A-Za-z0-9_\-\.]{2,}',sublinear_tf=True,max_df=0.5,min_df=2)
X=tv.fit_transform([' '.join(docs[k]) for k in range(K)]); voc=np.array(tv.get_feature_names_out())
ph=lambda t:'A_May' if t<'2026-06-11' else 'B_Jun11-17' if t<'2026-06-18' else 'C_Jun18' if t<'2026-06-19' else 'D_Jun19-22' if t<'2026-06-23' else 'E_coda'
revc=collections.Counter(); phc=collections.defaultdict(collections.Counter); labs=collections.defaultdict(set); fam=collections.defaultdict(collections.Counter)
for a in A:
    k=lab[idx[norm(a['add'])]]; revc[k]+=1; phc[k][ph(a['t'])]+=1; labs[k].add(a['label']); fam[k][a['fam']]+=1
random.seed(1)
out=[]
for k in sorted(range(K),key=lambda k:-revc[k]):
    top=voc[np.argsort(-X[k].toarray()[0])[:14]]
    samp=random.sample(docs[k],min(6,len(docs[k])))
    out.append(dict(k=int(k),revs=revc[k],uniq=len(docs[k]),labels=len(labs[k]),phase=dict(sorted(phc[k].items())),fam=fam[k].most_common(3),top=list(top),samples=[s[:160] for s in samp]))
json.dump(out,open(cache(f'clusters_k{K}.json'),'w'),indent=1)
for o in out:
    print(f"#{o['k']} revs={o['revs']} uniq={o['uniq']} labels={o['labels']} {o['phase']} fam={o['fam']}")
    print('   top:',' '.join(o['top']))
    for s in o['samples'][:4]: print('   -',s)
