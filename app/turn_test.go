package main

import (
	"fmt"
	"testing"
)

// A session that waits for the user blinks its card and the last message of its open chat, in one rhythm.
func TestYourTurnBlinks(t *testing.T) {
	e := newCoordinatorEnv(t)
	mine := e.mustKK("new", "claude", "-C", e.work, "hello")
	held := e.asKonduktor("new", "claude", "-C", e.work, "hello")
	busy := e.mustKK("new", "claude", "-C", e.work, "slow work")
	e.waitFor("a stop for the user", mine, func(c convView) bool { return turnEnded(c) && c.WaitingFor == waitingUser })
	e.waitFor("a stop for the konduktor", held, func(c convView) bool { return turnEnded(c) && c.WaitingFor == waitingKonduktor })
	e.waitFor("work", busy, func(c convView) bool { return c.Status == statusWorking })

	p := e.openPage(1600, 900)
	card := func(view, id string) string {
		return fmt.Sprintf(`document.querySelector("%s .tile[data-chain='%s']")`, view, id)
	}
	last := func(id string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#panel-%s .feed-inner > .msg")].at(-1)`, id)
	}
	blinks := func(el string) string {
		return fmt.Sprintf(`%s?.getAnimations().some((a) => a.animationName === "turn")`, el)
	}
	// at holds every blink at one moment of its period, so a screenshot shows one end of it.
	at := func(ms int) {
		p.eval(fmt.Sprintf(`document.getAnimations().filter((a) => a.animationName === "turn").forEach((a) => { a.pause(); a.currentTime = %d })`, ms), nil)
	}
	run := func() {
		p.eval(`document.getAnimations().filter((a) => a.animationName === "turn").forEach((a) => { a.startTime = 0 })`, nil)
	}
	still := func(els ...string) {
		t.Helper()
		for _, el := range els {
			var on bool
			p.eval(blinks(el)+" ?? false", &on)
			if on {
				t.Errorf("%s blinks", el)
			}
		}
	}

	p.until(blinks(card(".sidebar", mine)))
	p.until(card(".sidebar", held) + " && " + card(".sidebar", busy))
	still(card(".sidebar", held), card(".sidebar", busy))
	var period float64
	p.eval(card(".sidebar", mine)+`.getAnimations().find((a) => a.animationName === "turn").effect.getTiming().duration`, &period)
	if period != 2000 {
		t.Errorf("the blink takes %vms, want 2000", period)
	}
	var frame []string
	p.eval(fmt.Sprintf(`(() => { const el = %s, before = el.getBoundingClientRect().height; const colors = [0, 1000].map((ms) => {
		el.getAnimations().forEach((a) => { a.pause(); a.currentTime = ms }); return getComputedStyle(el).outlineColor })
		el.getAnimations().forEach((a) => a.play()); return [...colors, String(before === el.getBoundingClientRect().height)] })()`, card(".sidebar", mine)), &frame)
	if frame[0] == frame[1] || frame[2] != "true" {
		t.Errorf("the card does not blink its frame in place: %q", frame)
	}
	at(0)
	p.shot("turn panel card off")
	at(1000)
	p.shot("turn panel card on")
	run()

	p.click(`[...document.querySelectorAll(".views button")].find((b) => b.textContent === "Sessions")`)
	p.until(blinks(card(".board", mine)))
	p.until(card(".board", held) + " && " + card(".board", busy))
	still(card(".board", held), card(".board", busy))
	at(0)
	p.shot("turn board card off")
	at(1000)
	p.shot("turn board card on")
	run()

	p.click(`[...document.querySelectorAll(".views button")].find((b) => b.textContent === "Agents")`)
	p.openSession(held)
	p.until(last(held))
	still(last(held))
	p.openSession(mine)
	p.until(blinks(last(mine)) + " && " + card(".sidebar", mine) + `.classList.contains("on")`)
	var phases []float64
	p.eval(fmt.Sprintf(`[%s, %s].map((el) => el.getAnimations().find((a) => a.animationName === "turn").startTime)`, card(".sidebar", mine), last(mine)), &phases)
	if phases[0] != phases[1] {
		t.Errorf("the card and the message blink out of phase: %v", phases)
	}
	at(0)
	p.shot("turn message off")
	at(1000)
	p.shot("turn message on")
	run()

	p.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
	var steady []string
	p.eval(fmt.Sprintf(`[%s, %s].flatMap((el) => [String(%s), getComputedStyle(el).outlineColor])`, card(".sidebar", mine), last(mine), blinks("el")), &steady)
	if steady[0] != "false" || steady[2] != "false" || steady[1] != "rgb(255, 255, 255)" || steady[3] != steady[1] {
		t.Errorf("with reduced motion the card and the message are not steadily framed: %q", steady)
	}
	p.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{}})

	// Opening a waiting session acknowledges its wait until a new turn ends, also across a reload.
	unframed := func(els ...string) {
		t.Helper()
		still(els...)
		for _, el := range els {
			var style string
			p.eval(fmt.Sprintf(`getComputedStyle(%s).outlineStyle`, el), &style)
			if style != "none" {
				t.Errorf("%s is framed: %s", el, style)
			}
		}
	}
	// The headless timeline moves only on a new frame, so the messages could still be fading in on a screenshot.
	settle := func() {
		p.eval(`document.getAnimations().filter((a) => a.animationName === "enter").forEach((a) => a.finish())`, nil)
	}
	entering := func(el string) string {
		return fmt.Sprintf(`%s.getAnimations().find((a) => a.animationName === "enter")`, el)
	}
	p.eval(`window.entering = `+entering(last(mine)), nil)
	p.tap(card(".sidebar", mine))
	p.until(`!` + card(".sidebar", mine) + `.classList.contains("yours")`)
	unframed(card(".sidebar", mine), last(mine))
	var faded bool
	p.eval(`(`+entering(last(mine))+`) === window.entering`, &faded)
	if !faded {
		t.Error("the last message fades in again when its wait is acknowledged")
	}
	settle()
	p.shot("turn acknowledged")
	p.call("Page.reload", nil)
	p.until(card(".sidebar", mine) + " && " + last(mine))
	unframed(card(".sidebar", mine), last(mine))
	again := func(prompt string) {
		t.Helper()
		ended := e.conv(mine).TurnEnded
		e.mustKK("send", mine, prompt)
		e.waitFor("a new stop for the user", mine, func(c convView) bool {
			return turnEnded(c) && c.TurnEnded.After(ended) && c.WaitingFor == waitingUser
		})
		p.until(blinks(card(".sidebar", mine)) + " && " + blinks(last(mine)))
	}
	again("hello again")
	settle()
	at(1000)
	p.shot("turn blinks again")
	run()
	p.tap(fmt.Sprintf(`document.querySelector("#panel-%s textarea")`, mine))
	p.until(`!` + card(".sidebar", mine) + `.classList.contains("yours")`)
	unframed(card(".sidebar", mine), last(mine))
	again("hello once more")

	e.mustKK("send", mine, "slow work")
	e.waitFor("work again", mine, func(c convView) bool { return c.Status == statusWorking })
	p.until(`!(` + blinks(card(".sidebar", mine)) + `) && !(` + blinks(last(mine)) + `)`)

	// A konduktor waits for the user whenever it is idle, which is not a question to answer.
	side := `[...document.querySelectorAll(".side-panel .feed-inner > .msg")].at(-1)`
	e.mustKK("send", coordinatorID, "hello konduktor")
	e.waitFor("an idle konduktor", coordinatorID, func(c convView) bool {
		return hasEvent(c, kindAssistant, "echo: hello konduktor") && c.Status == statusIdle && c.WaitingFor == waitingUser
	})
	tab := func(title string) {
		p.click(fmt.Sprintf(`[...document.querySelectorAll("nav.tabs .dirs > div")].find((d) => d.title.startsWith(%q))`, title))
	}
	tab("Every directory")
	p.until(side + `?.textContent.includes("echo: hello konduktor")`)
	still(side, card("", coordinatorID))
	k := e.startKonduktor(e.work, "hello work")
	if c := e.conv(k); c.WaitingFor != waitingUser {
		t.Fatalf("the konduktor of the directory waits for %q", c.WaitingFor)
	}
	tab(e.work)
	p.until(side + `?.textContent.includes("echo: hello work")`)
	still(side, card("", k))
	p.shot("turn konduktor")
}
