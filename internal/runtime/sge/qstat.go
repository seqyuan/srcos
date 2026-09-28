package sge

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// JobState is SGE's view of a job, normalised.
//
// The distinction that matters most (ADR-015) is Suspended versus Gone: SGE
// preempts with SIGSTOP and may requeue, so a suspended job is still the user's
// work and must not be torn down as if it had died.
type JobState struct {
	// Raw is the two-letter SGE state as reported ("qw", "r", "s", "Eqw", ...).
	Raw string
	// Unknown means the scheduler does not know this job: it finished, was
	// deleted, or never existed.
	Unknown bool
	// Pending: queued, waiting for a slot.
	Pending bool
	// Running: has a slot.
	Running bool
	// Suspended: preempted or manually suspended. Alive, but not progressing.
	Suspended bool
	// Failed: an error state that needs a decision (Eqw, Eqw with a hold).
	Failed bool
}

// Alive reports whether the job still exists in the scheduler.
//
// Suspended counts as alive: it holds its slot and SGE may resume it. Treating
// it as dead is the mistake ADR-015 warns about.
func (s JobState) Alive() bool {
	return !s.Unknown && (s.Pending || s.Running || s.Suspended)
}

// Waiting reports whether the job has not started yet. On a busy cluster this
// can last hours, which is why the UI shows the queue position.
func (s JobState) Waiting() bool { return s.Pending }

func (s JobState) String() string {
	switch {
	case s.Unknown:
		return "unknown"
	case s.Failed:
		return "failed(" + s.Raw + ")"
	case s.Suspended:
		return "suspended"
	case s.Running:
		return "running"
	case s.Pending:
		return "pending"
	}
	return s.Raw
}

// classify maps an SGE state string onto JobState.
//
// SGE states seen in qstat -xml:
//
//	qw  queued, waiting            r   running
//	s   suspended (also `S`)       h   hold (user or system)
//	R   restarted                  d   deleted
//	E   error                      Eqw error, queued and waiting
//	t   transferring               T   threshold
//
// Unknown letters are preserved in Raw and treated as still-existing rather
// than silently mapped to a terminal state: guessing "dead" would make SRCOS
// abandon work that is actually in flight.
func classify(raw string) JobState {
	st := JobState{Raw: raw}
	body := strings.TrimSpace(raw)
	if body == "" {
		st.Unknown = true
		return st
	}
	// Eqw and friends: an error prefix means the job needs attention.
	if strings.HasPrefix(body, "E") {
		st.Failed = true
		return st
	}
	for _, c := range body {
		switch c {
		case 'q', 'w', 'h', 't', 'T':
			st.Pending = true
		case 'r', 'R':
			st.Running = true
		case 's', 'S':
			st.Suspended = true
		case 'd':
			// Deleted is on its way out; treat as gone so Wait stops polling.
			st.Unknown = true
			return st
		default:
			// An unrecognised letter keeps the job "alive" so nothing is
			// destroyed on a guess.
			st.Pending = true
		}
	}
	return st
}

// ─────────────────────────────────────────────────────────────────────────
// qstat -xml 解析
// ─────────────────────────────────────────────────────────────────────────

// qstatXML mirrors the subset of SGE's XML that SRCOS reads.
//
// SGE emits two containers: queue_info for jobs that hold a slot, and job_info
// for jobs that do not (queued, or already finished but still listed). SRCOS
// reads both, because a queued service is still a service.
type qstatXML struct {
	QueueInfo struct {
		Jobs []xmlJobList `xml:"job_list"`
	} `xml:"queue_info"`
	JobInfo struct {
		Jobs []xmlJobList `xml:"job_list"`
	} `xml:"job_info"`
}

type xmlJobList struct {
	State   string `xml:"state,attr"`
	JobNum  string `xml:"JB_job_number"`
	JobName string `xml:"JB_name"`
	Owner   string `xml:"JB_owner"`
	// The detailed state string (r/qw/s/Eqw). SGE puts the same value on the
	// attribute for most versions; the element wins when present.
	DetailedState string `xml:"state"`
	// Task id is present for array jobs; SRCOS does not submit array jobs but
	// reads it so a site that adds one later is not mis-parsed.
	TaskID string `xml:"JB_taskid"`
	Queue  string `xml:"queue_name"`
	Slots  string `xml:"slots"`
}

// Job is one parsed job.
type Job struct {
	ID     string
	Name   string
	Owner  string
	Queue  string
	Slots  int
	State  JobState
	TaskID string
}

// ParseQstatXML parses `qstat -xml` (with or without `-j`).
//
// It is deliberately tolerant: a scheduler that emits a partial document (which
// happens while jobs are transitioning) yields what it can rather than an
// error, because the caller's decision is "is this alive?" and a parse failure
// would answer that question with a wrong "no".
func ParseQstatXML(data string) ([]Job, error) {
	dec := xml.NewDecoder(strings.NewReader(data))
	dec.Strict = false

	var jobs []Job
	var cur *xmlJobList
	var stack []string

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if len(jobs) > 0 {
				return jobs, nil // partial document, but something usable
			}
			return nil, fmt.Errorf("parse qstat -xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			switch t.Name.Local {
			case "job_list":
				cur = &xmlJobList{}
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "state":
						cur.State = a.Value
					}
				}
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if t.Name.Local == "job_list" && cur != nil {
				if j, ok := cur.toJob(); ok {
					jobs = append(jobs, j)
				}
				cur = nil
			}
		case xml.CharData:
			if cur == nil || len(stack) == 0 {
				continue
			}
			// Only leaf elements carry scalars; the container names would
			// otherwise overwrite them with whitespace.
			switch stack[len(stack)-1] {
			case "JB_job_number":
				cur.JobNum = strings.TrimSpace(string(t))
			case "JB_name":
				cur.JobName = strings.TrimSpace(string(t))
			case "JB_owner":
				cur.Owner = strings.TrimSpace(string(t))
			case "state":
				cur.DetailedState = strings.TrimSpace(string(t))
			case "queue_name":
				cur.Queue = strings.TrimSpace(string(t))
			case "slots":
				cur.Slots = strings.TrimSpace(string(t))
			case "JB_taskid":
				cur.TaskID = strings.TrimSpace(string(t))
			}
		}
	}
	return jobs, nil
}

func (x *xmlJobList) toJob() (Job, bool) {
	if x.JobNum == "" {
		return Job{}, false // a job_list without an id is not a job
	}
	raw := x.DetailedState
	if raw == "" {
		raw = x.State
	}
	j := Job{
		ID:     x.JobNum,
		Name:   x.JobName,
		Owner:  x.Owner,
		Queue:  x.Queue,
		TaskID: x.TaskID,
		State:  classify(raw),
	}
	if n, err := parseLeadingInt(x.Slots); err == nil {
		j.Slots = n
	}
	return j, true
}

func parseLeadingInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	n := 0
	digits := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		digits++
	}
	if digits == 0 {
		return 0, fmt.Errorf("no leading integer in %q", s)
	}
	return n, nil
}

// FindJob returns the job with the given id.
func FindJob(jobs []Job, id string) (Job, bool) {
	for _, j := range jobs {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

// QueuePosition returns the job's 1-based position among pending jobs, which is
// what a user actually wants to see while waiting. Jobs are ordered by the
// scheduler's own sequence, so the index in the parsed list is the position.
func QueuePosition(jobs []Job, id string) (int, bool) {
	pos := 0
	for _, j := range jobs {
		if j.State.Pending {
			pos++
			if j.ID == id {
				return pos, true
			}
		}
	}
	return 0, false
}
