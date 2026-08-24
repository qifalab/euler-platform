module github.com/starcloud/sc-platform/sdk/terraform

go 1.26

require (
	github.com/hashicorp/terraform-plugin-framework v1.12.0
	github.com/starcloud/sc-platform v0.0.0
)

replace github.com/starcloud/sc-platform => ../../pkg-go
