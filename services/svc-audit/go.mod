module github.com/starcloud/sc-platform/services/svc-audit

go 1.26

require (
	github.com/google/uuid v1.6.0
	github.com/starcloud/sc-platform v0.0.0
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/go-sql-driver/mysql v1.9.0 // indirect
)

replace github.com/starcloud/sc-platform => ../../pkg-go
