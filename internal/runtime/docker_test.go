package runtime

import "testing"

func TestInferTCPPort(t *testing.T) {
	for _, tc := range []struct {
		exposed string
		want    int
	}{
		{`{"80/tcp":{}}`, 80}, {`{"53/udp":{},"8080/tcp":{}}`, 8080},
		{`null`, 0}, {`{}`, 0}, {`{"53/udp":{}}`, 0}, {`{"80/tcp":{},"443/tcp":{}}`, 0},
	} {
		got, err := inferTCPPort(tc.exposed)
		if got != tc.want || (err != nil) != (tc.want == 0) {
			t.Errorf("inferTCPPort(%s) = %d, %v", tc.exposed, got, err)
		}
	}
}
