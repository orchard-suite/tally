#!/usr/bin/env bash
# Tests claude-rotation against stand-ins for az and kubectl: the rotation
# lives in a file, the pin config map in another. Reading the active account
# plays tally: it takes the pin, or lets go of it when the account is spent
# (bo@example.com).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"

cat >"$work/bin/az" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE/az.log"
case "$*" in
*"secret list"*) echo 1 ;;
*"-n claude-rotation"*) cat "$FAKE/rotation" ;;
*"-n claude-token-dev-active"*)
	pin=$(cat "$FAKE/pin")
	case "$pin" in
	"") ;;
	bo@example.com) : >"$FAKE/pin" ;;
	*) echo "$pin" >"$FAKE/active" ;;
	esac
	jq -cn --arg a "$(cat "$FAKE/active")" '{account: $a, token: "sk-ant-x"}'
	;;
*) exit 1 ;;
esac
EOF
cat >"$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE/kubectl.log"
case "$*" in
*" patch configmap claude-pin "*) jq -r '.data.account' <<<"${@: -1}" >"$FAKE/pin" ;;
*" get configmap claude-pin "*) cat "$FAKE/pin" ;;
*) exit 1 ;;
esac
EOF
chmod +x "$work/bin/az" "$work/bin/kubectl"
echo '[{"account":"Ada@example.com","token":"sk-ant-a"},{"account":"bo@example.com","token":"sk-ant-b"}]' >"$work/rotation"
: >"$work/pin"
echo cy@example.com >"$work/active"

run() {
	env -i PATH="$work/bin:$PATH" HOME="$work" FAKE="$work" CLAUDE_ROTATION_VAULT=kv-example CLAUDE_ROTATION_AFTER='echo ran >>"$FAKE/after"' "$here/claude-rotation" "$@"
}
fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# pin takes the account's own spelling from the rotation.
out=$(run pin ada@EXAMPLE.com) || fail "pin exited non-zero"
[[ $(cat "$work/pin") == "Ada@example.com" ]] || fail "pin wrote '$(cat "$work/pin")'"
grep -q '^pinned: Ada@example.com$' <<<"$out" || fail "list after pin: $out"
grep -q -- '-n tally patch configmap claude-pin' "$work/kubectl.log" || fail "pin missed the tally namespace"
grep -q '^active: Ada@example.com$' <<<"$out" || fail "pin returned before Key Vault handed out the account: $out"
[[ $(wc -l <"$work/after") -eq 1 ]] || fail "the after command did not run once after pin"

# A spent account: tally lets go of the pin, and pin fails without running
# the after command.
if run pin bo@example.com >/dev/null 2>"$work/err"; then fail "pin said a spent account took"; fi
grep -q 'hit a limit' "$work/err" || fail "pin did not say the account is spent: $(cat "$work/err")"
[[ $(wc -l <"$work/after") -eq 1 ]] || fail "the after command ran for a pin that did not take"
echo Ada@example.com >"$work/pin"

# An account outside the rotation is refused and the pin stands.
if run pin eve@example.com 2>/dev/null; then fail "pin took an account outside the rotation"; fi
[[ $(cat "$work/pin") == "Ada@example.com" ]] || fail "a refused pin changed the pin"

# unpin empties it.
out=$(run unpin) || fail "unpin exited non-zero"
[[ -z $(cat "$work/pin") ]] || fail "unpin left '$(cat "$work/pin")'"
grep -q '^pinned: none$' <<<"$out" || fail "list after unpin: $out"
[[ $(wc -l <"$work/after") -eq 2 ]] || fail "the after command did not run after unpin"

# The settings file names the context; the environment wins over it.
mkdir -p "$work/.config/tally"
printf 'CLAUDE_ROTATION_CONTEXT=house\nCLAUDE_ROTATION_VAULT=kv-file\nNOT_OURS=x\n' >"$work/.config/tally/rotation.env"
rm -f "$work/kubectl.log" "$work/az.log"
run list >/dev/null || fail "list with a settings file exited non-zero"
grep -q -- '--context house' "$work/kubectl.log" || fail "the settings file's context was not used"
grep -q -- '--vault-name kv-file' "$work/az.log" && fail "the settings file won over the environment"
grep -q -- '--vault-name kv-example' "$work/az.log" || fail "the vault was not read"

echo "claude-rotation: ok"
