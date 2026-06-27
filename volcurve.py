import numpy as np, pandas as pd
from catboost import CatBoostRegressor
from mvp_perp_mm import build_day, add_features_target, FEATURES, TRAIN_DAYS, TEST_DAY, H_BINS
def prep(d):
    df=add_features_target(build_day(d)); r=np.log(df["mid"]).diff().fillna(0)
    df["logrv"]=np.log(np.sqrt((r**2).rolling(H_BINS).sum().shift(-H_BINS))*1e4+0.01)
    return df.dropna(subset=["logrv"])
tr=pd.concat([prep(d) for d in TRAIN_DAYS],ignore_index=True); te=prep(TEST_DAY)
yte=te["logrv"].values; v=np.var(yte)
print("curva de aprendizado (OOS Jun) — R2(log) vs fração do treino:")
for frac in (0.1,0.25,0.5,1.0):
    n=int(len(tr)*frac); s=tr.sample(n,random_state=1)
    m=CatBoostRegressor(iterations=600,learning_rate=0.03,depth=6,l2_leaf_reg=8,random_seed=42,verbose=0).fit(s[FEATURES],s["logrv"])
    p=m.predict(te[FEATURES]); r2=1-np.sum((yte-p)**2)/np.sum((yte-yte.mean())**2)
    print(f"  treino={n:>9,} ({frac*100:>4.0f}%)  R2(log)={r2:.4f}  corr={np.corrcoef(p,yte)[0,1]:.4f}")
