package cli

import (
	"github.com/c2tech-sys/cryobackup/internal/console/config"
)

// checkTrustedProxies prints one line naming trusted_proxies when the key is set, so the operator
// sees whose X-Forwarded-For the admin listener believes. Unset prints nothing (the default: no
// forwarded client address is believed). A list that is not valid is a FAIL of "config" already
// (config.Validate); this line then says it was not checked rather than repeat the problem.
func checkTrustedProxies(cfg config.Config, report func(level, name, detail string)) {
	const name = "trusted proxies"
	if len(cfg.TrustedProxies) == 0 {
		return
	}
	list := cfg.TrustedProxyList() // "" only when the list is not valid
	if list == "" {
		report("NOT CHECKED", name, "trusted_proxies is not valid (see config)")
		return
	}
	report("OK", name, "client address from X-Forwarded-For, and https from X-Forwarded-Proto, believed only from "+list+" (trusted_proxies)")
}
