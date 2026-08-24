# cn-east-1 prod — the remote standby region (两地三中心, 00§4.5; 09§5.2 M-8).
#
# The standby region's job (00§4.5): 只读流量分担 + 数据副本 + 冷备管控面. It
# takes NO writes — P3 不承诺异地多活写 (00§4.5). Concretely:
#
#   * data replicas run HOT — the cross-region replication channels (MySQL
#     binlog near-realtime, MinIO Replication async) land here, so RPO≈0 for
#     core ledger holds when the primary is lost.
#   * the control plane is COLD — services are scaled to zero until disaster
#     declaration, then 管控面冷转热 + DNS 切换 (00§4.5; 09§5.3 C1).
#
# Global vs regional (pkg-go/multiregion.Classify):
#   * GLOBAL singletons (svc-iam, svc-billing) run primary in cn-north-1 with a
#     cold primary-standby here (主备自动切换, 09§5.3 C1).
#   * REGIONAL services are replicated per region; their control plane is
#     cold here too, their data arrives via the replication channels.
#
# The per-service override files below are the pattern. In this repo's
# dir-as-env convention (08§4.3), a service without a file here falls back to
# its chart defaults (hot, replicas≥1) — which is the WRONG posture for a
# standby, so every control-plane service needs an explicit cold override when
# cn-east-1 is actually provisioned. The examples (svc-iam/svc-billing global,
# svc-catalog regional) demonstrate both classes.
