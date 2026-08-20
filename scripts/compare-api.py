# 由 compare-api.sh diff 调用：按解析后的值比对两次抓取的响应，忽略键序与易变字段。
import json, pathlib, sys
sp = pathlib.Path(sys.argv[1])
volatile = {"sampled_at_ms","last_poll","rx_bytes","tx_bytes","timestamp","session_rx_bytes","session_tx_bytes","session_total_bytes"}
def norm(o):
    if isinstance(o, dict):  return {k: norm(v) for k, v in sorted(o.items()) if k not in volatile}
    if isinstance(o, list):  return [norm(v) for v in o]
    return o
bad = 0
files = sorted((sp/"baseline").glob("*.json"))
for f in files:
    peer = sp/"after"/f.name
    if not peer.exists():
        bad += 1
        print("  ❌ %s：重构后不再有这个端点" % f.stem)
        continue
    a = json.loads(f.read_text() or "null")
    b = json.loads(peer.read_text() or "null")
    if norm(a) == norm(b): continue
    bad += 1
    print("  ❌ %s" % f.stem)
    print("     before:", json.dumps(norm(a), ensure_ascii=False)[:170])
    print("     after :", json.dumps(norm(b), ensure_ascii=False)[:170])
print("  ✅ %d 个端点响应全部一致" % len(files) if bad == 0 else "  %d 处不一致" % bad)

# 错误状态码：请求体不合法时该返回什么，和响应体一样属于对外行为。
codes = [(p/"_codes.txt").read_text() if (p/"_codes.txt").exists() else "" for p in (sp/"baseline", sp/"after")]
if codes[0] == codes[1]:
    print("  ✅ %d 个错误状态码一致" % len([l for l in codes[0].splitlines() if l.strip()]))
else:
    bad += 1
    print("  ❌ 错误状态码变了")
    print("     before:", codes[0].replace("\n", " | ").strip())
    print("     after :", codes[1].replace("\n", " | ").strip())
sys.exit(1 if bad else 0)
