import numpy as np, pandas as pd
from catboost import CatBoostRegressor
from mvp_perp_mm import build_day, add_features_target, FEATURES, TRAIN_DAYS, TEST_DAY
base={d:add_features_target(build_day(d)) for d in TRAIN_DAYS+[TEST_DAY]}
def withrv(df,H):
    df=df.copy(); r=np.log(df["mid"]).diff().fillna(0)
    df["logrv"]=np.log(np.sqrt((r**2).rolling(H).sum().shift(-H))*1e4+0.01)
    return df.dropna(subset=["logrv"])
print("R2(log) e corr da previsao de VOL por horizonte (OOS Jun):")
print(f"{'horizonte':>10} {'%mid=0':>7} {'R2(log)':>9} {'corr':>7}")
for H,lbl in [(10,'1s'),(50,'5s'),(150,'15s'),(300,'30s'),(600,'60s')]:
    tr=pd.concat([withrv(base[d],H) for d in TRAIN_DAYS],ignore_index=True); te=withrv(base[TEST_DAY],H)
    m=CatBoostRegressor(iterations=600,learning_rate=0.03,depth=6,l2_leaf_reg=8,random_seed=42,verbose=0).fit(tr[FEATURES],tr["logrv"])
    p=m.predict(te[FEATURES]); y=te["logrv"].values
    r2=1-np.sum((y-p)**2)/np.sum((y-y.mean())**2)
    zero=(np.exp(y)-0.01<1e-6).mean()*100
    print(f"{lbl:>10} {zero:>6.0f}% {r2:>9.4f} {np.corrcoef(p,y)[0,1]:>7.4f}")
