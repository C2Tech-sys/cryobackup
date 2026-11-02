package ui

import (
	"net/http"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
)

// The M2 customer and device routes (architecture §5.3; ticket M2-W1C). They are
// registered from one place so ui.go keeps a single additive line, and listed in
// m2Patterns so the route-coverage tests see them.
const (
	patCustomers        = "GET /customers"
	patCustomerNew      = "GET /ui/customers/new"
	patCustomerCreate   = "POST /ui/customers"
	patCustomer         = "GET /customers/{id}"
	patCustomerDevices  = "GET /customers/{id}/devices"
	patCustomerSettings = "GET /customers/{id}/settings"
	patCustomerUpdate   = "POST /ui/customers/{id}"
	patDevices          = "GET /devices"
	patDevicesTable     = "GET /ui/devices/table"
	patDeviceTab        = "GET /devices/{id}/{tab}"
	patDeviceSettings   = "POST /ui/devices/{id}/settings"
)

// m2Patterns lists what registerM2 adds. "GET /devices/{id}" (the Summary tab)
// is the M0 pattern, now served by the tabbed panel.
func m2Patterns() []string {
	return []string{patCustomers, patCustomerNew, patCustomerCreate, patCustomer, patCustomerDevices, patCustomerSettings, patCustomerUpdate,
		patDevices, patDevicesTable, patDeviceTab, patDeviceSettings}
}

// registerM2 adds the routes. Reads need read_only, a device's settings
// operator, and creating or editing a customer admin; each is CSRF-checked by
// Require when it is a write.
func (s *Service) registerM2(r httpx.Registrar) {
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeUI, h) }
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeUI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h) }
	r.Handle(patCustomers, ro(s.customers))
	r.Handle(patCustomerNew, ad(s.customerNew))
	r.Handle(patCustomerCreate, ad(s.customerCreate))
	r.Handle(patCustomer, ro(s.customerOverview))
	r.Handle(patCustomerDevices, ro(s.customerDevices))
	r.Handle(patCustomerSettings, ro(s.customerSettings))
	r.Handle(patCustomerUpdate, ad(s.customerUpdate))
	r.Handle(patDevices, ro(s.devicesPage))
	r.Handle(patDevicesTable, ro(s.devicesTablePartial))
	r.Handle(patDeviceTab, ro(s.devicePanel))
	r.Handle(patDeviceSettings, op(s.deviceSettingsPost))
}
