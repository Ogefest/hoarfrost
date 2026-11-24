package archive

import (
	"fmt"
	"log/slog"
	"time"
)

// progressInterval is how often long phases of sealing and verification
// log how far they are.
const progressInterval = time.Minute

// progressLog logs the progress of a long phase at most once per
// interval, with its rate and the time it still needs at that rate.
type progressLog struct {
	log      *slog.Logger
	msg      string
	interval time.Duration
	now      func() time.Time
	start    time.Time
	base     int64 // done when the phase started (a resumed phase)
	last     time.Time
}

func newProgressLog(log *slog.Logger, now func() time.Time, msg string, base int64) *progressLog {
	t := now()
	return &progressLog{log: log, msg: msg, interval: progressInterval, now: now, start: t, base: base, last: t}
}

func (p *progressLog) update(done, total int64, attrs ...any) {
	t := p.now()
	if t.Sub(p.last) < p.interval {
		return
	}
	p.last = t
	elapsed := t.Sub(p.start)
	attrs = append(attrs, "done", humanSize(done), "total", humanSize(total), "elapsed", elapsed.Round(time.Second))
	if total > 0 {
		attrs = append(attrs, "percent", fmt.Sprintf("%.1f", 100*float64(done)/float64(total)))
	}
	if rate := float64(done-p.base) / elapsed.Seconds(); rate > 0 {
		attrs = append(attrs, "rate", humanSize(int64(rate))+"/s")
		if total > done {
			attrs = append(attrs, "remaining", (time.Duration(float64(total-done)/rate) * time.Second).Round(time.Minute))
		}
	}
	p.log.Info(p.msg, attrs...)
}

// rate formats bytes moved in d as a rate for logs.
func rate(n int64, d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return humanSize(int64(float64(n)/d.Seconds())) + "/s"
}

// humanSize formats a byte count for logs.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
