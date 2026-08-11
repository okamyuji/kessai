# コスト最適化構成（`enable_budget_mode = true`）

学習・検証用途で常時起動しないケース向けの節約構成です。

## 変更点

| 項目 | 標準 | Budget | 月額差 |
|---|---|---|---|
| NAT Gateway | あり×1 | なし（ECSはpublicサブネットにpublic IP） | -$45 |
| Fargate | ON_DEMAND | SPOT capacity provider | 実効 -70% |
| RDS バックアップ保持 | 1日 | 0日 | 数百円 |
| ALB | 標準 | 標準（残す） | 変わらず |

## 有効化

`terraform.tfvars` に以下を追加します。

```hcl
enable_budget_mode = true
```

これで `network.tf` のNAT作成が抑制され、ECS `network_configuration.subnets` が publicサブネットに切り替わり、`assign_public_ip = true` になります。Fargate は Spot 100% になります。

## 注意点

- ECS タスクに public IP を付ける構成は、SGで inbound を ALB からのみ許可することで実質的な露出は変わりません。
- Fargate Spot は中断発生時にタスクが停止され、ECSが自動で置き換えます。決済処理中の中断でも Outbox がat-least-onceで再配送するため整合性は保たれます。
- RDS のバックアップ0はプロダクションでは非推奨（PITR不可）。学習中のみ。

## 使わない時間帯の停止

Terraform を残したまま以下で費用を最小化できます。

```bash
# ECSタスク数を0にする（Fargate料金停止）
aws ecs update-service --cluster kessai-dev --service kessai-dev --desired-count 0

# RDSインスタンス停止（7日で自動再起動される制限あり）
aws rds stop-db-instance --db-instance-identifier kessai-dev
```

完全撤退は `terraform destroy` で全リソース削除、次回は `terraform apply` で数分で復元できます。
