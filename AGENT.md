# AGENT.md — Cara pakai indostock sebagai tool AI trading

## Tool wrapper paling simple

Agent cukup exec binary, parse stdout JSON. Jangan parse stderr.

```python
import subprocess, json

def indostock(cmd, symbol, **opts):
    args = ["bin/indostock", cmd, symbol, "--compact"]
    if opts.get("depth"): args += ["--depth", str(opts["depth"])]
    if opts.get("interval"): args += ["--interval", opts["interval"]]
    if opts.get("range"): args += ["--range", opts["range"]]
    out = subprocess.check_output(args, stderr=subprocess.DEVNULL, text=True)
    return json.loads(out)

# contoh
quote = indostock("quote", "BBCA")
book = indostock("orderbook", "BBCA", depth=5)
snap = indostock("snapshot", "BBCA")  # paling hemat token: 1 call dapat quote+book+analysis
candles = indostock("history", "BBCA", interval="1d", range="1mo")
```

## Rekomendasi strategi tool-calling

1. **Snapshot dulu** — hemat 1 call untuk dapat sinyal awal (`analysis.signal`: bullish_pressure / bearish_pressure / bid_dominance / ask_dominance)
2. **History** jika butuh konfirmasi trend (MA, RSI dihitung agent sendiri dari candles)
3. **Orderbook depth 5-10** jika mau scalping / lihat imbalance ratio
4. **Watch** untuk monitoring realtime: `indostock orderbook BBCA --watch --interval 1 --compact` lalu tail NDJSON

## Handling orderbook tanpa token

Jika snapshot/orderbook return `{"error": "401..."}` berarti butuh STOCKBIT_TOKEN.

Agent bisa:
- fallback ke `quote` + `history` saja
- minta user: "Set env STOCKBIT_TOKEN dari stockbit.com (login -> DevTools -> Authorization header)"
- coba tanpa token kadang masih works untuk exodus endpoint saat jam bursa

## Compact vs Pretty

- Agent: selalu pakai `--compact` (1 baris, hemat token, mudah NDJSON streaming)
- Manusia: default pretty `--json` (indent 2)

