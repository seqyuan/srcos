package agenttoken

import (
	"time"

	"github.com/seqyuan/srcos/internal/activity"
)

// Usage records when each token was last used.
//
// It is a thin wrapper over package activity — the same write-behind journal the
// service reaper uses for "is this instance still in use" — because the
// mechanics are identical and the fact is equally low-value: a hint for a
// revocation decision, not an audit log.
//
// It lives in data/ rather than config/ on purpose: `srcos token create` runs as
// a separate process while the gateway may be answering requests, and a gateway
// write into agent-tokens.yaml could clobber a token the operator just created.
// Two files, two writers, no interleaving.
type Usage struct {
	journal *activity.Journal
}

// usageHeader explains the file to whoever opens it.
const usageHeader = "# SRCOS agent token usage（运行态，由网关写入）\n" +
	"# 记录每个 token 最近一次被使用的时间；`srcos token list` 会读它。\n" +
	"# 不是审计日志：只有时间戳，写入按 token 降频（首次使用即时落盘），\n" +
	"# 已撤销 token 的旧记录可能残留（按 id 合并，无害）。\n\n"

// LoadUsage reads the usage file. Damage means "no known usage", never a
// reason to refuse a request.
func LoadUsage(path string) *Usage {
	return &Usage{journal: activity.Load(path, usageHeader)}
}

// Last returns when a token was last used, or the zero time if unknown.
func (u *Usage) Last(id string) time.Time {
	if u == nil {
		return time.Time{}
	}
	return u.journal.Last(id)
}

// Touch records a use, writing the file when this token has not been written
// recently.
func (u *Usage) Touch(id string, at time.Time) error {
	if u == nil {
		return nil
	}
	return u.journal.Touch(id, at)
}

// Flush writes the usage file if anything changed.
func (u *Usage) Flush() error {
	if u == nil {
		return nil
	}
	return u.journal.Flush()
}
