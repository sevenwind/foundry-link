package main

import (
	"context"
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	histLen      = 120 // 2 минуты при одном замере в секунду
	lossWindow   = 60
	pongTimeout  = 3 * time.Second
	watchdogDead = 15 * time.Second
)

// Stats — пинг, джиттер и потери по одному каналу, измеряются QUIC-датаграммами.
// Датаграммы не стоят в очереди за потоком данных, поэтому пинг честный.
type Stats struct {
	mu       sync.Mutex
	rtt      float64 // сглаженный
	min      float64
	jitter   float64
	last     float64
	have     bool
	hist     []float64 // -1 = потеря
	outcomes []bool
	lastPong time.Time
}

type StatsView struct {
	Have        bool      `json:"have"`
	Ping        float64   `json:"ping"`
	Last        float64   `json:"last"`
	Min         float64   `json:"min"`
	Jitter      float64   `json:"jitter"`
	Loss        float64   `json:"loss"`
	Quality     string    `json:"quality"` // excellent | good | fair | poor | none
	QualityText string    `json:"qualityText"`
	History     []float64 `json:"history"`
}

func newStats() *Stats { return &Stats{lastPong: time.Now()} }

func (s *Stats) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rtt, s.min, s.jitter, s.last, s.have = 0, 0, 0, 0, false
	s.hist, s.outcomes = nil, nil
	s.lastPong = time.Now()
}

func (s *Stats) push(h float64, ok bool) {
	s.hist = append(s.hist, h)
	if len(s.hist) > histLen {
		s.hist = s.hist[len(s.hist)-histLen:]
	}
	s.outcomes = append(s.outcomes, ok)
	if len(s.outcomes) > lossWindow {
		s.outcomes = s.outcomes[len(s.outcomes)-lossWindow:]
	}
}

func (s *Stats) sample(rtt time.Duration) {
	ms := float64(rtt.Microseconds()) / 1000
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have {
		s.rtt, s.min, s.have = ms, ms, true
	} else {
		s.jitter += (math.Abs(ms-s.last) - s.jitter) / 8
		s.rtt += (ms - s.rtt) * 0.2
		if ms < s.min {
			s.min = ms
		}
	}
	s.last = ms
	s.lastPong = time.Now()
	s.push(ms, true)
}

func (s *Stats) loss() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.push(-1, false)
}

func (s *Stats) sinceLastPong() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastPong)
}

func (s *Stats) View() StatsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := StatsView{Have: s.have, Ping: round1(s.rtt), Last: round1(s.last), Min: round1(s.min), Jitter: round1(s.jitter),
		History: append([]float64(nil), s.hist...)}
	lost := 0
	for _, ok := range s.outcomes {
		if !ok {
			lost++
		}
	}
	if len(s.outcomes) > 0 {
		v.Loss = round1(float64(lost) * 100 / float64(len(s.outcomes)))
	}
	v.Quality, v.QualityText = quality(s.have, s.rtt, s.jitter, v.Loss)
	return v
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func quality(have bool, rtt, jitter, loss float64) (string, string) {
	switch {
	case !have:
		return "none", "Измеряю…"
	case rtt < 70 && jitter < 25 && loss <= 1:
		return "excellent", "Отлично"
	case rtt < 130 && jitter < 50 && loss <= 3:
		return "good", "Хорошо"
	case rtt < 220 && loss <= 8:
		return "fair", "Средне"
	}
	return "poor", "Плохо"
}

const (
	dgPing byte = 0xA1
	dgPong byte = 0xA2
)

// runPinger шлёт пинги раз в секунду и отвечает на пинги собеседника.
// Если понгов нет дольше watchdogDead, вызывает onDead (канал считается мёртвым).
func runPinger(ctx context.Context, conn quic.Connection, st *Stats, onDead func()) {
	var mu sync.Mutex
	sent := map[uint32]time.Time{}

	go func() {
		for {
			b, err := conn.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if len(b) != 5 {
				continue
			}
			seq := binary.BigEndian.Uint32(b[1:])
			switch b[0] {
			case dgPing:
				out := []byte{dgPong, 0, 0, 0, 0}
				binary.BigEndian.PutUint32(out[1:], seq)
				_ = conn.SendDatagram(out)
			case dgPong:
				mu.Lock()
				t0, ok := sent[seq]
				delete(sent, seq)
				mu.Unlock()
				if ok {
					st.sample(time.Since(t0))
				}
			}
		}
	}()

	started := time.Now()
	var seq uint32
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		seq++
		out := []byte{dgPing, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(out[1:], seq)
		mu.Lock()
		sent[seq] = time.Now()
		for k, t0 := range sent {
			if time.Since(t0) > pongTimeout {
				delete(sent, k)
				st.loss()
			}
		}
		mu.Unlock()
		_ = conn.SendDatagram(out)

		if time.Since(started) > watchdogDead && st.sinceLastPong() > watchdogDead {
			onDead()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
