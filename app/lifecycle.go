package main

import (
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Schedule tells on which weekdays and at which local times kk runs a conversation with its prompt.
type Schedule struct {
	// Days are weekdays, 0 for Sunday.
	Days []int `yaml:"days" json:"days"`
	// Times are local times of day, "15:04" or "15:04:05".
	Times  []string `yaml:"times" json:"times"`
	Prompt string   `yaml:"prompt" json:"prompt"`
	// Since is the run kk handled last, or when the schedule was set. Only later runs are due.
	Since time.Time `yaml:"since" json:"-"`
}

func parseClock(s string) (time.Time, bool) {
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// on returns the runs on the day of day, earliest first.
func (sc *Schedule) on(day time.Time) []time.Time {
	if !slices.Contains(sc.Days, int(day.Weekday())) {
		return nil
	}
	y, m, d := day.Date()
	var out []time.Time
	for _, s := range sc.Times {
		t, _ := parseClock(s)
		out = append(out, time.Date(y, m, d, t.Hour(), t.Minute(), t.Second(), 0, time.Local))
	}
	slices.SortFunc(out, time.Time.Compare)
	return out
}

// due returns the latest run of today that has come and is not handled yet. A run missed on an earlier day is not made up.
func (sc *Schedule) due(now time.Time) (time.Time, bool) {
	var due time.Time
	for _, t := range sc.on(now) {
		if !t.After(now) && t.After(sc.Since) {
			due = t
		}
	}
	return due, !due.IsZero()
}

// next returns the first run after now.
func (sc *Schedule) next(now time.Time) time.Time {
	for i := range 8 {
		for _, t := range sc.on(now.AddDate(0, 0, i)) {
			if t.After(now) {
				return t
			}
		}
	}
	return time.Time{}
}

// sleepAfter is how long a running agent may go without activity before kk stops it.
func sleepAfter() time.Duration { return envDuration("KK_SLEEP_AFTER", 24*time.Hour) }

// lifecycle puts agents that went quiet to sleep and starts the scheduled runs that are due.
func (s *Server) lifecycle(now time.Time) {
	after := sleepAfter()
	for _, c := range s.store.list() {
		if c.Closed || c.Role != "" {
			continue
		}
		if c.Schedule != nil {
			if slot, ok := c.Schedule.due(now); ok {
				s.run(c, slot)
				continue
			}
		}
		quiet := now.Sub(lastActive(c))
		if c.Status == statusIdle && quiet >= after && now.Sub(c.Launched) >= after && s.alive(c) && s.canResume(c) && midRewind(c.ID) == nil {
			s.sleep(c)
		}
	}
}

// sleep stops a quiet agent. The conversation stays open, waits for nobody and resumes like any stopped one.
func (s *Server) sleep(c Conversation) {
	s.controlled(c.ID)
	if err := hangUp(c.Window); err != nil {
		log.Printf("conversation %s: put to sleep: %v", c.ID, err)
		return
	}
	s.store.update(c.ID, func(c *Conversation) bool {
		c.Status, c.Attention, c.Sleeping = statusExited, "", true
		return true
	})
	s.publish(c.ID)
}

// run gives a scheduled conversation its prompt: typed into an idle agent, or with a resume of a stopped one. A busy agent
// is not interrupted; the run waits until it is idle, for the rest of the day.
func (s *Server) run(c Conversation, slot time.Time) {
	alive := s.alive(c)
	if alive && (c.Status != statusIdle || c.Attention != "" || midRewind(c.ID) != nil) {
		return
	}
	prompt := c.Schedule.Prompt
	// The user set the schedule, so what the run brings waits for them.
	c, _ = s.store.update(c.ID, func(c *Conversation) bool {
		sc := *c.Schedule
		sc.Since = slot
		c.Schedule, c.LastRun, c.ByAgent = &sc, time.Now(), false
		return true
	})
	log.Printf("conversation %s: scheduled run of %s", c.ID, slot.Format("Mon 15:04:05"))
	if alive {
		if err := pasteText(c.Window, prompt); err != nil {
			log.Printf("conversation %s: scheduled run: %v", c.ID, err)
		}
		s.publish(c.ID)
		return
	}
	resume := s.canResume(c)
	if !resume {
		c.Prompt = prompt
	}
	if err := s.launch(&c, resume, prompt); err != nil {
		log.Printf("conversation %s: scheduled run: %v", c.ID, err)
	}
}

func (s *Server) handleSchedule(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	var in Schedule
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if c.Role != "" {
		return nil, fail(400, "the conductor cannot be scheduled")
	}
	if c.Closed {
		return nil, fail(409, "the session is closed")
	}
	// A run would start a queued session ahead of its chain, without its prepared prompt.
	if c.Status == statusQueued {
		return nil, fail(409, "the session is queued, start it first")
	}
	sc := Schedule{Prompt: strings.TrimSpace(in.Prompt), Since: time.Now()}
	for _, d := range in.Days {
		if d < 0 || d > 6 {
			return nil, fail(400, "a day is 0 for Sunday to 6 for Saturday, not %d", d)
		}
		if !slices.Contains(sc.Days, d) {
			sc.Days = append(sc.Days, d)
		}
	}
	for _, t := range in.Times {
		if _, ok := parseClock(t); !ok {
			return nil, fail(400, "%q is not a time like 09:30", t)
		}
		if !slices.Contains(sc.Times, t) {
			sc.Times = append(sc.Times, t)
		}
	}
	slices.Sort(sc.Days)
	slices.Sort(sc.Times)
	switch {
	case len(sc.Days) == 0:
		return nil, fail(400, "pick at least one day")
	case len(sc.Times) == 0:
		return nil, fail(400, "add at least one time")
	case sc.Prompt == "":
		return nil, fail(400, "prompt is empty")
	}
	c, _ = s.store.update(c.ID, func(c *Conversation) bool {
		c.Schedule = &sc
		return true
	})
	s.publish(c.ID)
	return s.view(c, false), nil
}

func (s *Server) handleUnschedule(r *http.Request) (any, error) {
	c, err := s.conv(r)
	if err != nil {
		return nil, err
	}
	c, _ = s.store.update(c.ID, func(c *Conversation) bool {
		changed := c.Schedule != nil
		c.Schedule = nil
		return changed
	})
	s.publish(c.ID)
	return s.view(c, false), nil
}
