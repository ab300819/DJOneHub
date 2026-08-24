# 由 compare-api.sh diff 调用：按解析后的值比对两次抓取的响应，忽略键序与易变字段。
import json, pathlib, sys
sp = pathlib.Path(sys.argv[1])
volatile = {"sampled_at_ms","last_poll","rx_bytes","tx_bytes","timestamp","session_rx_bytes","session_tx_bytes","session_total_bytes"}
# 活流量列表：连接是机器此刻的实时快照，两次采集之间必然变化，比内容只会
# 造出误报。改为比结构——出现过哪些字段——这样"重构把 process 字段弄丢了"
# 仍然抓得到，而"微信换了一条连接"不会。
live_lists = {"connections"}
def shape(items):
    keys = sorted({k for it in items if isinstance(it, dict) for k in it})
    return {"__live_list_fields__": keys}
def norm(o):
    if isinstance(o, dict):
        out = {}
        for k, v in sorted(o.items()):
            if k in volatile:      continue
            if k in live_lists and isinstance(v, list): out[k] = shape(v)
            else:                  out[k] = norm(v)
        return out
    if isinstance(o, list):  return [norm(v) for v in o]
    return o
bad = 0

# A capture that died halfway used to leave a short baseline that every later
# comparison then passed, because the diff only walked the files that existed.
# The manifest is what the capture *intended* to collect, so a missing or empty
# response is a failure rather than a file that is quietly not compared.
def manifest(side):
    f = sp/side/"_manifest.txt"
    return [l.strip() for l in f.read_text().splitlines() if l.strip()] if f.exists() else []

want = {side: manifest(side) for side in ("baseline", "after")}
for side, names in want.items():
    if not names:
        print("  ❌ %s 没有 _manifest.txt：采集未完成，请重新抓取" % side)
        bad += 1
        continue
    missing = [n for n in names if not (sp/side/(n+".json")).exists() or not (sp/side/(n+".json")).read_text().strip()]
    if missing:
        bad += 1
        print("  ❌ %s 有 %d 个响应为空或缺失：%s" % (side, len(missing), " ".join(missing)))
if want["baseline"] and want["after"] and want["baseline"] != want["after"]:
    bad += 1
    print("  ❌ 两侧采集的端点集合不同，无法比对")
if bad:
    print("  采集不完整，比对结果不可信")
    sys.exit(1)

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
