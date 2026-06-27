import numpy as np, pandas as pd
from catboost import CatBoostRegressor
from mvp_perp_mm import build_day, add_features_target, FEATURES, TRAIN_DAYS, TEST_DAY
H=300  # alvo: vol forward 30s
def prep(d):
    df=add_features_target(build_day(d)).copy()
    r=np.log(df["mid"]).diff().fillna(0); r2=r**2
    for w,lbl in [(50,"rvb_5s"),(300,"rvb_30s"),(600,"rvb_60s"),(3000,"rvb_5m")]:
        df[lbl]=np.sqrt(r2.rolling(w,min_periods=2).sum())*1e4
    df["logrv"]=np.log(np.sqrt(r2.rolling(H).sum().shift(-H))*1e4+0.01)
    return df.dropna(subset=["logrv"]).fillna(0)
tr=pd.concat([prep(d) for d in TRAIN_DAYS],ignore_index=True); te=prep(TEST_DAY)
HARF=["rvb_5s","rvb_30s","rvb_60s","rvb_5m"]
def ev(feats,tag):
    m=CatBoostRegressor(iterations=600,learning_rate=0.03,depth=6,l2_leaf_reg=8,random_seed=42,verbose=0).fit(tr[feats],tr["logrv"])
    p=m.predict(te[feats]); y=te["logrv"].values
    r2=1-np.sum((y-p)**2)/np.sum((y-y.mean())**2)
    print(f"[{tag}] R2(log)={r2:.4f} corr={np.corrcoef(p,y)[0,1]:.4f}")
    return m
print("ALVO: vol forward 30s (OOS Jun)")
ev(FEATURES,"BASE  micro curto")
m=ev(FEATURES+HARF,"HAR   + vol 5s/30s/60s/5m")
print("importancia (top 6):")
for f,v in sorted(zip(FEATURES+HARF,m.get_feature_importance()),key=lambda x:-x[1])[:6]:
    print(f"  {f:10s} {v:.1f}")
