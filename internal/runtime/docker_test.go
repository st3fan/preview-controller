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

func TestMappedDockerPort(t *testing.T) {
	for _, tc := range []struct {
		name, active string
		want         int
	}{
		{"selected target", `{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"31001"}],"80/tcp":null}`, 31001},
		{"wrong target", `{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"31001"}]}`, 0},
		{"wrong protocol", `{"8080/udp":[{"HostIp":"127.0.0.1","HostPort":"31001"}]}`, 0},
		{"public binding", `{"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"31001"}]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := `{"running":true,"requested":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":""}]},"active":` + tc.active + "}"
			got, err := mappedDockerPort(data)
			if got != tc.want || (err != nil) != (tc.want == 0) {
				t.Fatalf("got %d, %v", got, err)
			}
		})
	}
}
