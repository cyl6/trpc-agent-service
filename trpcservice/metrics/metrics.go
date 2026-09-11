package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Metrics is a small dependency-free Prometheus exporter. Labels are limited
// to tenant/component/result; request, user, session and trace IDs are banned
// to keep cardinality bounded.
type Metrics struct {
	mu     sync.RWMutex
	values map[string]float64
	help   map[string]string
	types  map[string]string
}

func NewMetrics() *Metrics {
	return &Metrics{values: make(map[string]float64), help: make(map[string]string), types: make(map[string]string)}
}

func (m *Metrics) Add(name, help string, value float64, labels map[string]string) {
	key := metricKey(name, labels)
	m.mu.Lock()
	m.values[key] += value
	m.help[name] = help
	m.types[name] = "counter"
	m.mu.Unlock()
}

// Set records the current value of a gauge, replacing the previous sample for
// the same bounded label set.
func (m *Metrics) Set(name, help string, value float64, labels map[string]string) {
	key := metricKey(name, labels)
	m.mu.Lock()
	m.values[key] = value
	m.help[name] = help
	m.types[name] = "gauge"
	m.mu.Unlock()
}

func (m *Metrics) Observe(name, help string, value float64, labels map[string]string) {
	m.Add(name+"_sum", help, value, labels)
	m.Add(name+"_count", help, 1, labels)
	m.mu.Lock()
	m.types[name+"_sum"] = "gauge"
	m.types[name+"_count"] = "counter"
	m.mu.Unlock()
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.help))
	for name := range m.help {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, m.help[name], name, m.types[name])
		keys := make([]string, 0)
		for key := range m.values {
			if key == name || strings.HasPrefix(key, name+"{") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = io.WriteString(w, key+" "+strconv.FormatFloat(m.values[key], 'f', -1, 64)+"\n")
		}
	}
}

func metricKey(name string, labels map[string]string) string {
	allowed := []string{"tenant", "component", "result", "channel", "backend"}
	parts := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, ok := labels[key]; ok && value != "" {
			parts = append(parts, key+"=\""+escapeLabel(value)+"\"")
		}
	}
	if len(parts) == 0 {
		return name
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return strings.ReplaceAll(value, "\"", "\\\"")
}
