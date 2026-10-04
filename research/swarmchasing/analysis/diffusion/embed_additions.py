"""Embed the unique normalised wiki additions with all-MiniLM-L6-v2 (sentence-transformers).
Writes cache/E.npy and cache/uniq.json. Usage: uv run --with sentence-transformers --with scikit-learn python embed_additions.py
"""
import os,json,re,collections,random
from _posts import cache, load_additions
os.environ['OMP_NUM_THREADS']='8'; os.environ['TOKENIZERS_PARALLELISM']='false'
import numpy as np, torch
torch.set_num_threads(8)
from sentence_transformers import SentenceTransformer
from sklearn.cluster import KMeans
from sklearn.feature_extraction.text import TfidfVectorizer
A=load_additions()
def norm(s):
    s=re.sub(r'https?://([A-Za-z0-9.-]+)\S*',lambda m:' URL_'+m.group(1).replace('.','_')+' ',s)
    s=re.sub(r'\b\d{6,}\b',' NUM ',s)
    return re.sub(r'\s+',' ',s).strip()[:500]
texts=[norm(a['add']) for a in A]
uniq=sorted(set(texts)); idx={t:i for i,t in enumerate(uniq)}
print('uniq',len(uniq),flush=True)
m=SentenceTransformer('all-MiniLM-L6-v2'); m.max_seq_length=128
E=m.encode(uniq,batch_size=64,show_progress_bar=False,normalize_embeddings=True)
np.save(cache('E.npy'),E); json.dump(uniq,open(cache('uniq.json'),'w'))
print('embedded',flush=True)
