package execution

import (
	"strings"

	"github.com/numofx/market-maker/internal/config"
)

func isProtectedOrderID(cfg config.Config, orderID string) bool {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" || len(cfg.ProtectedOrderIDPrefixes) == 0 {
		return false
	}
	for _, prefix := range cfg.ProtectedOrderIDPrefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(orderID, prefix) {
			return true
		}
	}
	return false
}
