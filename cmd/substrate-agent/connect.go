package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/spawnly/platform/internal/attestor"
	"github.com/spawnly/platform/internal/substrate/channel"
)

// dialer holds the channel connection parameters.
type dialer struct {
	channelURL string // ws(s)://host/v1/substrate/channel
	audience   string
	source     attestor.Source
	heartbeat  time.Duration
	backoffMin time.Duration
	backoffMax time.Duration
	// reconcile is the period of the reconcile safety-net (remote-substrate
	// Phase 5): in addition to reconcile-on-connect, a stably-connected substrate
	// re-reconciles on this interval so a live kill/spawn it somehow missed still
	// converges WITHOUT waiting for a reconnect. 0 ⇒ the 60s default.
	reconcile time.Duration
}

func (d dialer) withDefaults() dialer {
	if d.heartbeat <= 0 {
		d.heartbeat = 30 * time.Second
	}
	if d.backoffMin <= 0 {
		d.backoffMin = time.Second
	}
	if d.backoffMax <= 0 {
		d.backoffMax = 30 * time.Second
	}
	if d.reconcile <= 0 {
		d.reconcile = 60 * time.Second
	}
	return d
}

// serve holds the outbound channel, reconnecting with capped exponential backoff
// until ctx is cancelled. Each successful connection resets the backoff; a failed
// dial or a dropped connection grows it. The whole substrate→CP path is
// client-initiated (NAT-friendly, outbound-only).
func (a *agent) serve(ctx context.Context, d dialer) {
	d = d.withDefaults()
	backoff := d.backoffMin
	for ctx.Err() == nil {
		connected, err := a.connectOnce(ctx, d)
		if err != nil && ctx.Err() == nil {
			log.Printf("substrate-agent: channel error: %v", err)
		}
		if connected {
			backoff = d.backoffMin // a real connection happened; reset.
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff = backoff * 2; backoff > d.backoffMax {
			backoff = d.backoffMax
		}
	}
}

// connectOnce dials, authenticates with a fresh SVID, reconciles on connect, and
// pumps frames until the connection ends. connected reports whether the dial
// succeeded (so the caller can reset backoff).
func (a *agent) connectOnce(ctx context.Context, d dialer) (connected bool, err error) {
	cred, err := d.source.Fetch(ctx, d.audience)
	if err != nil {
		return false, err
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+cred.Value)

	c, _, err := websocket.Dial(ctx, d.channelURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return false, err
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	log.Printf("substrate-agent: channel connected (org=%s)", a.org)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sendCh := make(chan channel.Up, 256)
	a.setSend(func(u channel.Up) {
		select {
		case sendCh <- u:
		default: // never block a handler on a slow socket
		}
	})
	defer a.setSend(nil)

	// Writer: drain UP frames to the socket.
	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case u := <-sendCh:
				wctx, wcancel := context.WithTimeout(connCtx, 15*time.Second)
				werr := wsjson.Write(wctx, c, u)
				wcancel()
				if werr != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Heartbeat: liveness ping so the server's read-deadline never expires on an
	// idle-but-live channel.
	go func() {
		t := time.NewTicker(d.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-t.C:
				a.report(channel.NewHeartbeat())
			}
		}
	}()

	// Reconcile desired-vs-actual on (re)connect — the durable backstop — and run
	// it to COMPLETION before consuming any live DOWN frame. If it ran concurrently
	// with the reader (as a goroutine), a spawn pushed right after connect could
	// race the reconcile snapshot: its record appears in `desired` (preregistered)
	// while its just-created CR is not yet in `actual`, so reconcile would create a
	// duplicate — or worse, a CR created by a live handleSpawn between reconcile's
	// two list reads would be seen as "actual not desired" and torn down. Draining
	// reconcile first makes the live stream strictly post-reconcile. Heartbeats keep
	// flowing meanwhile (that goroutine is already running), so the server's read
	// deadline never expires while reconcile does its I/O.
	if rerr := a.reconcileOnce(connCtx); rerr != nil && connCtx.Err() == nil {
		log.Printf("substrate-agent: reconcile-on-connect: %v", rerr)
	}

	// Periodic reconcile safety-net: reconcile-on-connect already converges a
	// reconnecting substrate, but a substrate that stays connected for a long time
	// and misses a single live kill/spawn (e.g. a frame lost when a buffer-full
	// server-side drop was immediately re-established) would otherwise never
	// converge until its next reconnect. A low-frequency periodic reconcile closes
	// that gap. It is serialized with live spawn/kill via a.opMu (inside
	// reconcileOnce), so it never races a dispatch.
	go func() {
		t := time.NewTicker(d.reconcile)
		defer t.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-t.C:
				if rerr := a.reconcileOnce(connCtx); rerr != nil && connCtx.Err() == nil {
					log.Printf("substrate-agent: periodic reconcile: %v", rerr)
				}
			}
		}
	}()

	// Reader: consume DOWN frames until the connection ends.
	c.SetReadLimit(1 << 20)
	for {
		var down channel.Down
		rerr := wsjson.Read(connCtx, c, &down)
		if rerr != nil {
			return true, rerr
		}
		if verr := down.Validate(); verr != nil {
			log.Printf("substrate-agent: dropping malformed down frame: %v", verr)
			continue
		}
		a.handleDown(connCtx, down)
	}
}
