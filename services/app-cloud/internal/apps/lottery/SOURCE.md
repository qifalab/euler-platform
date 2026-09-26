# Source and licensing

The feature baseline is [miaojilab/emoera-lottery-system](https://github.com/miaojilab/emoera-lottery-system/tree/ab25a69d2678072a3608d36a3a0a2dd8d9971123), Apache License 2.0, Copyright 2026 Miaoji Lab. Its full license is preserved in `LICENSE.emoera-lottery-system`.

Euler independently implements the original room, participant, signup, draw, results and history workflows in Go and Vue. The original Next.js/MySQL service is neither run nor modified. Euler replaces browser-local ownership and anonymous management with authenticated project scope, and replaces the random-comparator algorithm and unbound pool transactions with `crypto/rand` sampling and one database transaction. See `docs/unified-app-cloud/ACTIVITY-APPS.md` for parity and intentional changes.

QR encoding uses `github.com/skip2/go-qrcode`, version pinned in the application's Go module; its BSD-3-Clause license is retained by the dependency distribution. No third-party QR HTTP service receives invitation URLs.
