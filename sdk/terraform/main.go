// starcloud-terraform-provider — the Terraform Provider for the platform
// (09-roadmap §5.1 goal 3, §5.2 M-10; 三期验收 C3: Terraform Provider 覆盖核心
// 产品).
//
// This is the phase-3 "IaC 消费者一等公民" surface: every resource maps to a
// product OpenAPI action, signed with the SAME cps1 implementation the Go SDK,
// Python SDK, gateway verifier and OpenAPI Explorer share (pkg-go/cps1, via
// pkg-go/scsdk) — 03§9.4 rule ⑤: one contract, many consumers. The provider
// never re-implements signing; drift is caught by the golden-vector fixture,
// not by Terraform users.
//
// Source-only deliverable (repo convention): the framework dependency is not
// vendored on this host, so this module is complete and statically consistent
// but not built. The four core products (SCECS/SCOSS/SCVPC/SCRDS) are covered
// as resource skeletons sharing one CRUD helper; the pattern is the deliverable
// the acceptance criterion (C3) counts.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/starcloud/sc-platform/sdk/terraform/internal/provider"
)

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New, providerserver.ServeOpts{
		Address: "registry.terraform.io/starcloud/starcloud",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
