package proto

// BundleScript runs as the workload user in its checkout. The first
// argument selects HEAD-only pushes or checkpoints; the second is the
// output path. Checkpoints also take the previous ref list (empty for the
// first checkpoint). The remaining arguments name operation states.
const BundleScript = `set -e
mode=$1; b=$2; shift 2
previous=
if [ "$mode" = checkpoint ]; then previous=$1; shift; fi
check() {
	for s in "$@"; do
		p=$(git rev-parse --git-path "$s")
		if [ -e "$p" ] || [ -L "$p" ]; then
			printf 'lux-operation:%s\n' "$s"
			exit 0
		fi
	done
}
refs() {
	git for-each-ref --format='%(objectname) %(refname)' refs/heads/
	printf '%s HEAD\n' "$(git rev-parse --verify HEAD)"
}
check "$@"
case "$mode" in
push) S=$(git rev-parse --verify HEAD) ;;
checkpoint)
	S=$(refs)
	if [ "$S" = "$previous" ]; then echo lux-unchanged; exit 0; fi
	;;
*) exit 2 ;;
esac
check "$@"
case "$mode" in
push) git bundle create --quiet "$b" HEAD ;;
checkpoint) git bundle create --quiet "$b" --branches HEAD ;;
esac
check "$@"
heads=$(git bundle list-heads "$b")
case "$mode" in
push)
	if [ "$heads" != "$S HEAD" ]; then echo lux-moved; fi
	;;
checkpoint)
	if [ "$heads" != "$S" ] || [ "$(refs)" != "$S" ]; then echo lux-moved; fi
	;;
esac`
