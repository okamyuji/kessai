# ADR-0017 AWSデプロイ構成を標準構成とBudget構成の切替式にする

## ステータス

Accepted（2026-08-11）

## コンテキスト

学習用のkessaiをAWS上で実際に動作させるため、Terraformによる構成を用意する必要があります。実装は`deploy/terraform/`にまとめます。当初は本番指向の高可用性構成（NAT Gateway・複数AZ・Fargate ON_DEMAND）で設計しましたが、学習用途では常時起動しない前提でコストが月$90を超えるのは過大です。運用途中で構成を切り替えられるよう、両者を1つのTerraformモジュール内で`enable_budget_mode`変数で選択できる形式にします。

## 決定

`deploy/terraform/`は次の共通要素を持ちます。VPC（2AZ、public/private各2サブネット）、ALB（HTTPリスナ、public配置）、ECS Fargate、ECR、RDS PostgreSQL 18.4（db.t4g.micro、gp3 20GB、private配置、暗号化）、Secrets Manager、CloudWatch Logs（14日保持）。

`enable_budget_mode`変数で以下を切り替えます。

| 項目 | 標準構成（`false`、既定） | Budget構成（`true`） |
|---|---|---|
| NAT Gateway | あり×1（`aws_nat_gateway`+EIP） | なし（`count=0`） |
| ECS配置サブネット | privateサブネット | publicサブネット |
| ECS public IP | 割り当てなし | 割り当てあり |
| プライベートルート | NAT宛の`0.0.0.0/0` | なし（RDSはprivate内で完結） |
| Fargate capacity | `FARGATE`（ON_DEMAND） | `FARGATE_SPOT` |
| RDSバックアップ | 1日 | 0日 |
| 概算コスト | 約$90/月 | 約$25/月 |

RDSは両構成ともprivateサブネット配置のままとし、パブリック露出しません（ECSからのSGルールのみで到達）。

## 根拠

1. NAT Gatewayは月$45と最大の費用要因で、単独で標準構成の半分を占めます。学習用途では東西トラフィックが少ないため、ECSタスクをpublicサブネットに置いてpublic IP経由でStripeとECRへ届かせる構成でも実質的な露出はSGで抑制できます（inboundはALBのSG発のみ許可）。
2. Fargate SPOTは中断リスクがある一方、Outboxリレーがat-least-once配送を保証し、状態遷移テーブルと`transfer_id`の一意制約が二重処理を排除する設計になっているため（ADR-0007、ADR-0008、ADR-0009）、中断時にもデータ整合性が壊れません。学習用途では約70%の実効コスト削減が得られます。
3. RDSバックアップ0はプロダクションでは推奨できませんが、学習中はスキーマ変更や再作成が頻繁で、PITRの必要性は低いためBudget時のみ無効化します。
4. 両構成を`enable_budget_mode`変数で切り替える方式にすることで、学習中はBudget、負荷試験や本番相当検証時は標準構成、と用途で使い分けられます。`terraform apply -var enable_budget_mode=true`だけで差分反映されます。
5. 標準構成をディフォルトにしたのは、うっかりSPOT中断や単一障害点構成で本番運用してしまう事故を避けるためです。コスト最適化は明示的なオプトインとします。

## 影響

- `variables.tf`に`enable_budget_mode`を追加、`network.tf`のNAT関連リソースと`private`ルート表を`count = var.enable_budget_mode ? 0 : 1/az_count`で条件生成。
- `ecs.tf`で`aws_ecs_cluster_capacity_providers`とサービスの`capacity_provider_strategy`をBudget切替対応。ECSサービスの`network_configuration.subnets`と`assign_public_ip`も同変数で切替。
- `rds.tf`の`backup_retention_period`も切替。
- 運用手順とコスト表を`deploy/terraform/budget.md`に記載。
- Terraformの`validate`は両モードで成功、`plan`はAWS認証情報が必要になります。

## 検討した代替案

- Aurora Serverless v2のスケール0: 2024年時点でAurora Serverless v2はscale to zeroが未対応のため見送り。将来対応時に再評価。
- NAT Instance（EC2でNATを自前運用）: 月$5程度まで削減できるが、可用性とパッチ運用の負担が学習効果に見合わないため見送り。将来的にVPCエンドポイント（ECR/Secrets Manager/S3）に切り替える案も候補。
- App Runner: マネージド度が高い代わりに、ECS/Fargateの学習範囲が狭まるため学習目的に反する。
- EC2 + docker-compose: 最安構成だが、ECS/Fargate/ALBの実運用経験を学ぶ機会が失われる。
- 単一構成にしてBudgetを既定: 本番運用へ移行する際にオプトアウトが必要になり、意図しない構成事故を防ぎにくいため見送り。
