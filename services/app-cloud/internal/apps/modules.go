// Package apps composes the independent Euler application suite. No original
// product management service or original product database is used at runtime.
package apps

import (
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/database"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/eid"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/lottery"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/statistics"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/storage"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/trust"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/weauth"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/apps/witshield"
)

func New(rt *appkit.Runtime) []appkit.Module {
	return []appkit.Module{trust.New(rt), eid.New(rt), weauth.New(rt), database.New(rt), storage.New(rt), statistics.New(rt), lottery.New(rt), witshield.New(rt)}
}
