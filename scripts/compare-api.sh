#!/bin/sh
# 重构护栏：对比重构前后 API 行为是否一致。
#
# 用法：
#   scripts/compare-api.sh baseline <重构前的提交>   # 建基线
#   scripts/compare-api.sh after                     # 抓当前
#   scripts/compare-api.sh diff                      # 比对
#
# 基线用 git worktree 单独构建，因此不受工作区状态影响。响应体按解析后的值比对
# （忽略键序），时间戳和字节计数这类每次都变的字段排除在外。
set -eu

MODE=${1:?usage: compare-api.sh baseline <ref> | after | diff}
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
WORK="${TMPDIR:-/tmp}/djonehub-api-compare"
PORT=7801

capture() {
  binary=$1
  outdir=$2
  mkdir -p "$outdir"

  # A capture that dies before its kill leaves a server holding the port, and
  # every later capture then silently talks to that stale process instead of the
  # binary it was asked to measure — which reads as a behaviour difference that
  # is not one. Refuse to start, and clean up even when interrupted.
  if lsof -ti ":$PORT" >/dev/null 2>&1; then
    echo "端口 $PORT 已被占用，先清理：kill \$(lsof -ti :$PORT)" >&2
    exit 1
  fi

  "$binary" -demo -listen "127.0.0.1:$PORT" >/dev/null 2>&1 &
  pid=$!
  trap 'kill $pid 2>/dev/null || true' EXIT INT TERM
  i=0
  while [ $i -lt 20 ]; do
    curl -s -m 2 "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 && break
    sleep 1
    i=$((i + 1))
  done

  : > "$outdir/_manifest.txt"

  # nettop needs half a minute per call when it resolves names, so the timeout
  # has to clear that. A curl failure is recorded rather than fatal: aborting
  # mid-capture used to leave a partial baseline that the diff then accepted.
  for ep in health status sms sms/status network network/traffic network/local \
            network/activity esim esim/health esim/notes esim/module-notes; do
    name=$(echo "$ep" | tr / _)
    echo "$name" >> "$outdir/_manifest.txt"
    curl -s -m 90 "http://127.0.0.1:$PORT/api/$ep" > "$outdir/$name.json" || true
  done

  post() {
    echo "${3:?post needs a capture name}" >> "$outdir/_manifest.txt"
    curl -s -m 90 -X POST "http://127.0.0.1:$PORT/api/$1" \
      -H 'Content-Type: application/json' -d "$2" || true
  }
  post at '{"command":"AT+CSQ"}' p_at                    > "$outdir/p_at.json"
  post sms/send '{"phone":"10086","message":"hi"}' p_sms_send  > "$outdir/p_sms_send.json"
  post sms/refresh '{}' p_sms_refresh                             > "$outdir/p_sms_refresh.json"
  post sms/clear-module '{}' p_sms_clear                        > "$outdir/p_sms_clear.json"
  post network/usbnet '{"mode":1}' p_usbnet                   > "$outdir/p_usbnet.json"
  post network/check-4g '{}' p_check4g                         > "$outdir/p_check4g.json"
  post esim/switch '{"iccid":"898601"}' p_esim_switch              > "$outdir/p_esim_switch.json"
  post esim/download '{"smdp":"rsp.example.com","imei":"123456789012345"}' p_esim_dl > "$outdir/p_esim_dl.json"
  post esim/phonebook/probe '{}' p_phonebook                     > "$outdir/p_phonebook.json"

  code() {
    curl -s -m 90 -o /dev/null -w '%{http_code}' -X "$1" "http://127.0.0.1:$PORT/api/$2" \
      -H 'Content-Type: application/json' -d "$3" || true
  }
  {
    printf 'at_bad %s\n'       "$(code POST at '{"command":"HELLO"}')"
    printf 'sms_empty %s\n'    "$(code POST sms/send '{"phone":"","message":""}')"
    printf 'usbnet_bad %s\n'   "$(code POST network/usbnet '{"mode":9}')"
    printf 'note_bad %s\n'     "$(code PUT esim/notes '{}')"
    printf 'esim_del_bad %s\n' "$(code DELETE esim/profile '{}')"
  } > "$outdir/_codes.txt"

  kill $pid 2>/dev/null || true
  wait $pid 2>/dev/null || true
  trap - EXIT INT TERM
}

case "$MODE" in
  baseline)
    REF=${2:?usage: compare-api.sh baseline <ref>}
    rm -rf "$WORK/tree" "$WORK/baseline"
    git -C "$ROOT" worktree remove --force "$WORK/tree" 2>/dev/null || true
    git -C "$ROOT" worktree add -q -f "$WORK/tree" "$REF"
    (cd "$WORK/tree" && ./scripts/build-macos.sh >/dev/null)
    capture "$WORK/tree/dist/djonehub-macos" "$WORK/baseline"
    git -C "$ROOT" worktree remove --force "$WORK/tree"
    echo "baseline captured from $REF -> $WORK/baseline"
    ;;
  after)
    rm -rf "$WORK/after"
    capture "$ROOT/dist/djonehub-macos" "$WORK/after"
    echo "current captured -> $WORK/after"
    ;;
  diff)
    python3 "$ROOT/scripts/compare-api.py" "$WORK"
    ;;
  *)
    echo "unknown mode: $MODE" >&2
    exit 1
    ;;
esac
