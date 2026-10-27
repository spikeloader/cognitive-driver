package driver

import (
	"testing"

	"github.com/spikeloader/cognitive-driver/internal/config"
)

func TestAttributeDefaults(t *testing.T) {
	for _, tc := range []struct {
		m    config.Metric
		want string
	}{
		{config.Metric{Operand: config.Operand{Name: "node_cpu_seconds_total"}}, "cpu"},
		{config.Metric{Operand: config.Operand{Name: "node_memory_MemAvailable_bytes"}}, "memory"},
		{config.Metric{Operand: config.Operand{Name: "node_disk_io_time_seconds_total"}}, "io"},
		{config.Metric{Operand: config.Operand{Name: "node_cpu_seconds_total"}, Attribute: "none"}, ""},
		{config.Metric{Operand: config.Operand{Name: "nginx_connections_active"}}, ""},
		{config.Metric{Operand: config.Operand{Name: "nginx_connections_active"}, Attribute: "cpu"}, "cpu"},
	} {
		if got := attributeFor(tc.m); got != tc.want {
			t.Errorf("attributeFor(%s, %q) = %q, esperado %q", tc.m.Key(), tc.m.Attribute, got, tc.want)
		}
	}
}
