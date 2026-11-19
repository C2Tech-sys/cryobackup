package state

// HeartbeatPingFailuresToWarn is how many pings in a row must fail before the console says so.
// At the default five-minute period that is about fifteen minutes.
const HeartbeatPingFailuresToWarn = 3

// heartbeatBannerID is the id of the outbound-heartbeat banner; the "not checked" variant adds "b".
const heartbeatBannerID = "C19"

// HeartbeatFacts is the state of the optional outbound heartbeat ping (ADR 0020 decision 12): a
// request the console sends to an operator-chosen URL, so an outside monitor notices a console
// that died. The jobs package keeps these between passes and supplies them; this package only
// reads them. A nil *HeartbeatFacts means the loader does not supply them, and raises nothing.
type HeartbeatFacts struct {
	// Configured is true when the operator set a URL. Without one the feature is off and nothing is
	// raised: it is optional, so its absence is not a fault.
	Configured bool
	// LastSuccessAt is when the newest ping was answered with a 2xx; zero when none has been since
	// the console started.
	LastSuccessAt Millis
	// ConsecutiveFailures counts the pings sent that failed since the last success. A ping that was
	// withheld is not a failure and does not count.
	ConsecutiveFailures int
	// LastFailureClass is the class word of the newest failed ping: timeout, dns, refused, tls,
	// status_4xx, status_5xx, redirect, bad_config or other. Never a URL and never reply text.
	LastFailureClass string
	// Withheld is true when the newest pass sent nothing because the alarm-evaluator is not healthy.
	// The missing ping is the alarm there, raised by the outside monitor, and the evaluator
	// banners (C06) already say why, so this one stays quiet.
	Withheld bool
}

// heartbeatClasses are the class words a banner may repeat. Anything else, empty included, is
// shown as "other": the text of a banner never carries a value the facts loader did not vet.
var heartbeatClasses = map[string]bool{
	"timeout": true, "dns": true, "refused": true, "tls": true,
	"status_4xx": true, "status_5xx": true, "redirect": true, "bad_config": true, "other": true,
}

// heartbeatBanners is C19: the outbound heartbeat ping failed HeartbeatPingFailuresToWarn times in
// a row. At most one banner. Nothing while no URL is configured or while the ping is withheld.
func heartbeatBanners(h *HeartbeatFacts, _ bannerCtx) []Banner {
	if h == nil || !h.Configured {
		return nil
	}
	if h.ConsecutiveFailures < 0 {
		return []Banner{notChecked(heartbeatBannerID+"b", "External heartbeat ping",
			"a failing ping would go unnoticed", "its failure count is garbled", 0)}
	}
	if h.Withheld || h.ConsecutiveFailures < HeartbeatPingFailuresToWarn {
		return nil
	}
	class := h.LastFailureClass
	if !heartbeatClasses[class] {
		class = "other"
	}
	return []Banner{{ID: heartbeatBannerID, Severity: SeverityAlarm, Count: h.ConsecutiveFailures,
		Text: "The external heartbeat ping is failing (" + class + "): an outside monitor may report this console as down."}}
}
