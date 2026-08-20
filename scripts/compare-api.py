# 由 compare-api.sh diff 调用：按解析后的值比对两次抓取的响应，忽略键序与易变字段。
import json, pathlib, sys
sp = pathlib.Path(sys.argv[1])
volatile = {"sampled_at_ms","last_poll","rx_bytes","tx_bytes","timestamp","session_rx_bytes","session_tx_bytes","session_total_bytes"}
def norm(o):
    if isinstance(o, dict):  return {k: norm(v) for k, v in sorted(o.items()) if k not in volatile}
    if isinstance(o, list):  return [norm(v) for v in o]
    return o
bad = 0
for f in sorted((sp/"baseline").glob("*.json")):
    a = json.loads(f.read_text() or "null")
    b = json.loads((sp/"after"/f.name).read_text() or "null")
    if norm(a) == norm(b): continue
    bad += 1
    print("  ❌ %s" % f.stem)
    print("     before:", json.dumps(norm(a), ensure_ascii=False)[:170])
    print("     after :", json.dumps(norm(b), ensure_ascii=False)[:170])
print("  ✅ 21 个端点响应全部一致" if bad == 0 else "  %d 处不一致" % bad)
