# kessai AWS Terraform デプロイ

## 構成

- VPC（`/16`、AZ×2、public/private サブネット各1組、NAT×1でコスト抑制）
- ALB（HTTPのみ、ヘルスチェック `GET /`）
- ECS Fargate（`0.5vCPU/1GB`、desired_count=1、awsvpcネットワーク）
- ECR（scan_on_push、直近5世代保持）
- RDS PostgreSQL 18.4（`db.t4g.micro`、gp3 20GB、private配置、暗号化）
- Secrets Manager（DB URL/Stripeキー/セッションキーをまとめて保存、タスクに環境変数として注入）
- CloudWatch Logs（`/ecs/kessai-dev`、14日保持）

## 変数

`terraform.tfvars` に少なくとも以下を設定してください。

```hcl
stripe_secret_key      = "sk_test_xxxxx"
stripe_publishable_key = "pk_test_xxxxx"
stripe_webhook_secret  = "whsec_xxxxx"
admin_email            = "admin@example.com"
session_signing_key    = "please-generate-32bytes-random-string!!"
```

## 手順

```bash
# 1. 初期化とプラン
terraform init
terraform validate
terraform plan -out=plan.tfplan

# 2. 適用（AWSリソースが作成される。料金発生）
terraform apply plan.tfplan

# 3. アプリのイメージを ECR へ push（terraform 出力の repository_url を使う）
aws ecr get-login-password --region ap-northeast-1 \
  | docker login --username AWS --password-stdin $(terraform output -raw ecr_repository_url)
docker build -t kessai:latest ../..
docker tag kessai:latest $(terraform output -raw ecr_repository_url):latest
docker push $(terraform output -raw ecr_repository_url):latest

# 4. ECS サービスの再デプロイ（新イメージを反映）
aws ecs update-service --cluster $(terraform output -raw cluster_name) --service kessai-dev --force-new-deployment

# 5. 動作確認
curl -w "\ntime=%{time_total}s\n" http://$(terraform output -raw alb_dns)/

# 6. マイグレーション適用（初回のみ）
# ローカルからRDSへ直接届かないため、bastion か ecs exec 経由で `migrate` を実行するか、
# アプリのentrypointに migrations 実行を組み込む拡張が必要。
```

## 撤退

```bash
terraform destroy
```

課金対象: ALB、NAT Gateway、RDSインスタンス、CloudWatch Logsが主。学習用途では常時起動を避け、使わないときは `terraform destroy` を推奨します。
