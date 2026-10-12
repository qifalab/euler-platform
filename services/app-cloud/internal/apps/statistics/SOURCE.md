# Source and licensing

The feature baseline is [ctipscn/ecloud-statistics](https://github.com/ctipscn/ecloud-statistics/tree/30ad3c2d7ded5eae059dd1380be91595ddbf1bbc), licensed under Apache License 2.0. Its license is preserved in `LICENSE.ecloud-statistics`.

Euler independently implements the baseline's page collector, PV/UV dashboard, URL ranking, pagination, automatic refresh, embeddable counter and callback/Promise page-count helper in Go and Vue. It does not run or change the original Express/Pug service. The original global administration token is replaced by Euler's authenticated project scope. Site-scoped publication IDs, exact origin restrictions and identifier minimization are Euler additions. See `docs/unified-app-cloud/ACTIVITY-APPS.md` for the exact parity and security boundaries.
