"""
ENGINE de MM cross-venue reversao (Estrategia 4), Paradex SOL vs fair value Binance.
FASE 1 = DRY_RUN: roda contra o feed AO VIVO, sem key/dinheiro. Cota bid/ask maker em torno
do fair value quando o basis disloca, simula fill quando um trade real cruza nosso quote
(fill OTIMISTA front-of-queue — a fila real so o live com key mede), e mede o markout 5s.
Loga tudo. Vira live setando DRY_RUN=0 + as keys (hooks place/cancel/fills via paradex-py).

Feeds publicos: Binance bookTicker (fair) + Paradex bbo + trades. Zero secret no codigo.
ENV: DRY_RUN=1  SYMBOL=SOL  SIZE=... (contratos)  THR_BP=20  MARKOUT_S=5  EMA_MIN=3
     MAX_OPEN=1  DATABASE_URL(opcional log)  PARADEX_L1/L2 keys (so live)
"""
from __future__ import annotations
import os, json, time, threading, ssl, math
import websocket

DRY = os.environ.get("DRY_RUN", "1") != "0"
COIN = os.environ.get("SYMBOL", "SOL").upper()
SIZE = float(os.environ.get("SIZE", "1"))
THR_BP = float(os.environ.get("THR_BP", "20"))
MARKOUT_MS = int(float(os.environ.get("MARKOUT_S", "5")) * 1000)
EMA_MIN = float(os.environ.get("EMA_MIN", "3"))
MAX_OPEN = int(os.environ.get("MAX_OPEN", "1"))
PDX_SYM = f"{COIN}-USD-PERP"; BNC_SYM = f"{COIN.lower()}usdt"

S = {"bnc_mid": None, "pdx_bid": None, "pdx_ask": None, "pdx_bidsz": 0.0, "pdx_asksz": 0.0,
     "basis": None, "eq": None, "quote": None, "open": [], "n_fill": 0, "pnl_rev": []}
lock = threading.Lock()

def now_ms(): return int(time.time() * 1000)
def log(ev, **kw): print(json.dumps({"t": now_ms(), "ev": ev, **kw})[:400], flush=True)

def update_basis():
    if S["bnc_mid"] and S["pdx_bid"] and S["pdx_ask"]:
        pmid = (S["pdx_bid"] + S["pdx_ask"]) / 2
        b = (math.log(pmid) - math.log(S["bnc_mid"])) * 1e4
        S["basis"] = b
        if S["eq"] is None: S["eq"] = b
        else:
            # EMA causal ~EMA_MIN min; tick ~ ate 10/s -> alpha pequeno
            alpha = 1 - math.exp(-0.1 / (EMA_MIN * 60))
            S["eq"] = alpha * b + (1 - alpha) * S["eq"]

# ---------- feeds ----------
def bnc_feed():
    url = f"wss://fstream.binance.com/stream?streams={BNC_SYM}@bookTicker"
    while True:
        try:
            ws = websocket.create_connection(url, timeout=10, sslopt={"cert_reqs": ssl.CERT_NONE}); ws.settimeout(30)
            while True:
                d = json.loads(ws.recv()).get("data")
                if d and d.get("b") and d.get("a"):
                    with lock: S["bnc_mid"] = (float(d["b"]) + float(d["a"])) / 2; update_basis()
        except Exception as e: log("bnc_reconnect", err=str(e)[:60]); time.sleep(2)

def pdx_feed():
    url = "wss://ws.api.prod.paradex.trade/v1"
    while True:
        try:
            ws = websocket.create_connection(url, timeout=10, sslopt={"cert_reqs": ssl.CERT_NONE})
            for i, ch in enumerate([f"bbo.{PDX_SYM}", f"trades.{PDX_SYM}"]):
                ws.send(json.dumps({"jsonrpc": "2.0", "method": "subscribe", "params": {"channel": ch}, "id": i + 1}))
            ws.settimeout(30)
            while True:
                m = json.loads(ws.recv())
                if m.get("method") != "subscription": continue
                ch = m["params"]["channel"]; d = m["params"]["data"]
                if ch.startswith("bbo"):
                    if d.get("bid") and d.get("ask"):
                        with lock:
                            S["pdx_bid"] = float(d["bid"]); S["pdx_ask"] = float(d["ask"])
                            S["pdx_bidsz"] = float(d.get("bid_size") or 0); S["pdx_asksz"] = float(d.get("ask_size") or 0)
                            update_basis()
                elif ch.startswith("trades"):
                    handle_trade(float(d["price"]), float(d["size"]), str(d.get("side", "")).upper())
        except Exception as e: log("pdx_reconnect", err=str(e)[:60]); time.sleep(2)

# ---------- fill sim (DRY) ----------
def handle_trade(px, sz, side):
    """trade real da Paradex: side B=taker buy(lift ask)->nosso ASK fila; A=taker sell(hit bid)->nosso BID."""
    with lock:
        q = S["quote"]
        if not q or not DRY: return
        filled = None
        if q["side"] == "bid" and side.startswith("A") and px <= q["price"] + 1e-9: filled = "bid"
        if q["side"] == "ask" and side.startswith("B") and px >= q["price"] - 1e-9: filled = "ask"
        if filled and len(S["open"]) < MAX_OPEN:
            d = +1 if filled == "bid" else -1
            pos = {"t": now_ms(), "d": d, "px": q["price"], "basis0": S["basis"], "eq0": S["eq"]}
            S["open"].append(pos); S["n_fill"] += 1; S["quote"] = None
            log("FILL_SIM", side=filled, px=q["price"], dev=round(S["basis"] - S["eq"], 1), size=SIZE)
            threading.Timer(MARKOUT_MS / 1000, lambda: markout(pos)).start()

def markout(pos):
    with lock:
        if S["basis"] is None: return
        rev = pos["d"] * (S["basis"] - pos["basis0"])   # reversao capturada (bps, fair value)
        S["pnl_rev"].append(rev)
        if pos in S["open"]: S["open"].remove(pos)
        arr = S["pnl_rev"]
        log("MARKOUT", rev_bp=round(rev, 2), n=len(arr), mean_bp=round(sum(arr) / len(arr), 2),
            pos=round(sum(1 for x in arr if x > 0) / len(arr) * 100))

# ---------- engine tick ----------
def engine():
    while True:
        time.sleep(0.1)
        with lock:
            if S["basis"] is None or S["eq"] is None: continue
            dev = S["basis"] - S["eq"]
            if len(S["open"]) >= MAX_OPEN: S["quote"] = None; continue
            if dev < -THR_BP:      # Paradex barata -> quota BID (compra, espera reverter p/ cima)
                S["quote"] = {"side": "bid", "price": S["pdx_bid"]}
            elif dev > THR_BP:     # Paradex cara -> quota ASK
                S["quote"] = {"side": "ask", "price": S["pdx_ask"]}
            else:
                S["quote"] = None
            # LIVE: aqui chamaria place_order/cancel via paradex-py (Fase 1b)

def heartbeat():
    while True:
        time.sleep(30)
        with lock:
            arr = S["pnl_rev"]
            log("HB", basis=round(S["basis"] or 0, 1), eq=round(S["eq"] or 0, 1),
                fills=S["n_fill"], markouts=len(arr),
                mean_rev=round(sum(arr) / len(arr), 2) if arr else None, dry=DRY)

if __name__ == "__main__":
    log("START", coin=COIN, dry=DRY, thr_bp=THR_BP, size=SIZE, markout_s=MARKOUT_MS / 1000)
    if not DRY:
        log("LIVE_MODE", note="place/cancel/fills via paradex-py — hooks a implementar na Fase 1b")
    for fn in (bnc_feed, pdx_feed, engine, heartbeat):
        threading.Thread(target=fn, daemon=True).start()
    while True: time.sleep(3600)
