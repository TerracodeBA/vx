package metrics

import (
	"strings"
	"testing"
)

func TestMetricsText(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("t_requests_total", "Requests.\nTwo lines.", "route", "status")
	c.Inc("/ui", "200")
	c.Add(2, "/ui", "200")
	c.Inc(`we"ird\`, "500")
	g := r.NewGauge("t_plain", "A gauge without labels.")
	_ = g
	h := r.NewHistogram("t_seconds", "Latency.", []float64{0.1, 1}, "route")
	h.Observe(0.05, "/a")
	h.Observe(0.5, "/a")
	h.Observe(5, "/a")
	r.SetGaugeFunc("t_queue", "Queue depth.", func() []Sample { return []Sample{{Labels: []string{"forecast"}, Value: 3}} }, "queue")
	r.SetGaugeFunc("t_queue", "Queue depth.", func() []Sample { return []Sample{{Labels: []string{"forecast"}, Value: 4}} }, "queue")

	var b strings.Builder
	r.WriteText(&b)
	out := b.String()
	for _, want := range []string{
		"# HELP t_requests_total Requests.\\nTwo lines.\n# TYPE t_requests_total counter\n",
		`t_requests_total{route="/ui",status="200"} 3` + "\n",
		`t_requests_total{route="we\"ird\\",status="500"} 1` + "\n",
		"# TYPE t_plain gauge\nt_plain 0\n",
		`t_seconds_bucket{route="/a",le="0.1"} 1` + "\n",
		`t_seconds_bucket{route="/a",le="1"} 2` + "\n",
		`t_seconds_bucket{route="/a",le="+Inf"} 3` + "\n",
		`t_seconds_sum{route="/a"} 5.55` + "\n",
		`t_seconds_count{route="/a"} 3` + "\n",
		`t_queue{queue="forecast"} 4` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "# TYPE t_queue") != 1 {
		t.Errorf("replaced gauge func listed twice:\n%s", out)
	}
	if c.Value("/ui", "200") != 3 || h.Count("/a") != 3 {
		t.Errorf("Value/Count wrong: %v %v", c.Value("/ui", "200"), h.Count("/a"))
	}
}

func TestMetricsBoundSeries(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("t_bounded_total", "Bounded.", "v")
	for i := 0; i < maxSeries+50; i++ {
		c.Inc(strings.Repeat("x", i%7) + string(rune('a'+i%26)) + itoa(i))
	}
	if got := len(c.series); got > maxSeries+1 {
		t.Fatalf("series = %d, want at most %d", got, maxSeries+1)
	}
	if c.Value("other") != 50 {
		t.Fatalf("other = %v, want 50", c.Value("other"))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestMetricsDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("t_dup", "x")
	defer func() {
		if recover() == nil {
			t.Fatal("registering a name twice did not panic")
		}
	}()
	r.NewGauge("t_dup", "y")
}
