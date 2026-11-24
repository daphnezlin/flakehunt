package main

// ServiceConfig is a dependency the tests talk to (an api, a db...). Each trial
// gets its own copy behind its own toxiproxy, on a private docker network:
//
//	test container --> toxiproxy:8666 --(latency added here)--> service:<Port>
//
// so trials can't slow each other's network down.
type ServiceConfig struct {
	Image  string
	Cmd    []string
	Port   int
	Health string   // shell command that succeeds once the service is up, optional
	Env    []string // set on the test container, "{addr}" becomes the proxy address
}

// Jitter is how much latency varies per response. Real networks aren't
// perfectly steady, and some spread means a tight timeout fails sometimes
// rather than always.
func Jitter(latencyMS int) int { return latencyMS / 2 }
