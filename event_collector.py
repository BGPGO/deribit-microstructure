"""
Coletor EVENT-LEVEL (Binance lider + Paradex seguidor) -> Postgres. Sem grid de 100ms:
cada update do book/trade no seu timestamp real de exchange. Pro sinal, depois amostra-se
a feature no instante de cada decisao com o ultimo estado de cada venue (preserva lead sub-200ms).

Eventos:
  Binance: bookTicker (top-of-book a cada mudanca) + aggTrade (se a venue/IP permitir)
  Paradex: bbo.{m} (top a cada mudanca, event-level) + trades.{m}

Tabelas (criadas no startup):
  book_events(venue,sym,ts_ms,bid,bidsz,ask,asksz)
  trade_events(venue,sym,ts_ms,px,sz,side)

Config por ENV (nada hardcoded):
  DATABASE_URL  (postgres://user:pass@host:5432/db)   OBRIGATORIO
  COINS=SOL     (csv)
  FLUSH_S=1
"""
from __future__ import annotations
import os, json, time, threading, ssl
import websocket
import psycopg2
from psycopg2.extras import execute_values

DB = os.environ["DATABASE_URL"]
COINS = [c.strip().upper() for c in os.environ.get("COINS", "SOL").split(",")]
FLUSH_S = float(os.environ.get("FLUSH_S", "1"))

buf_book, buf_trade = [], []
lock = threading.Lock()
cnt = {"binance": {"book": 0, "trade": 0}, "paradex": {"book": 0, "trade": 0}}

DDL = """
CREATE TABLE IF NOT EXISTS book_events(
  venue text, sym text, ts_ms bigint, bid float8, bidsz float8, ask float8, asksz float8);
CREATE TABLE IF NOT EXISTS trade_events(
  venue text, sym text, ts_ms bigint, px float8, sz float8, side char(1));
CREATE INDEX IF NOT EXISTS ix_book ON book_events(sym, ts_ms);
CREATE INDEX IF NOT EXISTS ix_trade ON trade_events(sym, ts_ms);
"""

def db_init():
    c = psycopg2.connect(DB); c.autocommit = True
    with c.cursor() as cur: cur.execute(DDL)
    c.close()

def flusher():
    conn = psycopg2.connect(DB); conn.autocommit = True
    while True:
        time.sleep(FLUSH_S)
        with lock:
            bk, tr = buf_book[:], buf_trade[:]
            buf_book.clear(); buf_trade.clear()
        try:
            with conn.cursor() as cur:
                if bk: execute_values(cur, "INSERT INTO book_events(venue,sym,ts_ms,bid,bidsz,ask,asksz) VALUES %s", bk)
                if tr: execute_values(cur, "INSERT INTO trade_events(venue,sym,ts_ms,px,sz,side) VALUES %s", tr)
        except Exception as e:
            print("flush err:", str(e)[:120]); time.sleep(2)
            try: conn = psycopg2.connect(DB); conn.autocommit = True
            except: pass

def add_book(v, s, ts, b, bs, a, as_):
    with lock: buf_book.append((v, s, int(ts), b, bs, a, as_)); cnt[v]["book"] += 1
def add_trade(v, s, ts, px, sz, side):
    with lock: buf_trade.append((v, s, int(ts), px, sz, side)); cnt[v]["trade"] += 1

# ---------- Binance ----------
def binance():
    streams = "/".join(f"{c.lower()}usdt@bookTicker/{c.lower()}usdt@aggTrade" for c in COINS)
    url = f"wss://fstream.binance.com/stream?streams={streams}"
    rev = {f"{c}USDT": c for c in COINS}
    while True:
        try:
            ws = websocket.create_connection(url, timeout=10, sslopt={"cert_reqs": ssl.CERT_NONE})
            ws.settimeout(30)
            while True:
                m = json.loads(ws.recv()); d = m.get("data"); st = m.get("stream", "")
                if not d: continue
                if st.endswith("@bookTicker"):
                    s = rev.get(d.get("s", ""))
                    if s: add_book("binance", s, d.get("T") or d.get("E") or time.time()*1000,
                                   float(d["b"]), float(d["B"]), float(d["a"]), float(d["A"]))
                elif st.endswith("@aggTrade"):
                    s = rev.get(d.get("s", ""))
                    if s: add_trade("binance", s, d.get("T"), float(d["p"]), float(d["q"]), "A" if d.get("m") else "B")
        except Exception as e:
            print("binance reconnect:", str(e)[:80]); time.sleep(2)

# ---------- Paradex ----------
def paradex():
    url = "wss://ws.api.prod.paradex.trade/v1"
    sym = {c: f"{c}-USD-PERP" for c in COINS}; rev = {v: k for k, v in sym.items()}
    while True:
        try:
            ws = websocket.create_connection(url, timeout=10, sslopt={"cert_reqs": ssl.CERT_NONE})
            i = 0
            for c in COINS:
                for ch in (f"bbo.{sym[c]}", f"trades.{sym[c]}"):
                    i += 1; ws.send(json.dumps({"jsonrpc": "2.0", "method": "subscribe", "params": {"channel": ch}, "id": i}))
            ws.settimeout(30)
            while True:
                m = json.loads(ws.recv())
                if m.get("method") != "subscription": continue
                p = m["params"]; ch = p["channel"]; d = p["data"]; s = rev.get(d.get("market", ""))
                if not s: continue
                if ch.startswith("bbo"):
                    b, a = d.get("bid"), d.get("ask")
                    if b and a: add_book("paradex", s, d.get("last_updated_at") or time.time()*1000,
                                         float(b), float(d.get("bid_size") or 0), float(a), float(d.get("ask_size") or 0))
                elif ch.startswith("trades"):
                    side = "B" if str(d.get("side", "")).upper().startswith("B") else "A"
                    add_trade("paradex", s, d.get("created_at"), float(d["price"]), float(d["size"]), side)
        except Exception as e:
            print("paradex reconnect:", str(e)[:80]); time.sleep(2)

def reporter():
    while True:
        time.sleep(30); print(f"[{time.strftime('%H:%M:%S')}] eventos: {cnt}")

if __name__ == "__main__":
    db_init()
    for fn in (flusher, binance, paradex, reporter):
        threading.Thread(target=fn, daemon=True).start()
    while True: time.sleep(3600)
