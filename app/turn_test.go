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
	// at holds every blink at one moment of its second, so a screenshot shows one end of it.
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
	if period != 1000 {
		t.Errorf("the blink takes %vms, want 1000", period)
	}
	var frame []string
	p.eval(fmt.Sprintf(`(() => { const el = %s, before = el.getBoundingClientRect().height; const colors = [0, 500].map((ms) => {
		el.getAnimations().forEach((a) => { a.pause(); a.currentTime = ms }); return getComputedStyle(el).outlineColor })
		el.getAnimations().forEach((a) => a.play()); return [...colors, String(before === el.getBoundingClientRect().height)] })()`, card(".sidebar", mine)), &frame)
	if frame[0] == frame[1] || frame[2] != "true" {
		t.Errorf("the card does not blink its frame in place: %q", frame)
	}
	at(0)
	p.shot("turn panel card off")
	at(500)
	p.shot("turn panel card on")
	run()

	p.click(`[...document.querySelectorAll(".views button")].find((b) => b.textContent === "Sessions")`)
	p.until(blinks(card(".board", mine)))
	p.until(card(".board", held) + " && " + card(".board", busy))
	still(card(".board", held), card(".board", busy))
	at(0)
	p.shot("turn board card off")
	at(500)
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
	at(500)
	p.shot("turn message on")
	run()

	p.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
	var steady []string
	p.eval(fmt.Sprintf(`[%s, %s].flatMap((el) => [String(%s), getComputedStyle(el).outlineColor])`, card(".sidebar", mine), last(mine), blinks("el")), &steady)
	if steady[0] != "false" || steady[2] != "false" || steady[1] != "rgb(255, 255, 255)" || steady[3] != steady[1] {
		t.Errorf("with reduced motion the card and the message are not steadily framed: %q", steady)
	}
	p.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{}})

	e.mustKK("send", mine, "slow work")
	e.waitFor("work again", mine, func(c convView) bool { return c.Status == statusWorking })
	p.until(`!(` + blinks(card(".sidebar", mine)) + `) && !(` + blinks(last(mine)) + `)`)
}
