variable "aws_region" {
  description = "デプロイ先のAWSリージョン"
  type        = string
  default     = "ap-northeast-1"
}

variable "environment" {
  description = "環境名（dev/prod等）"
  type        = string
  default     = "dev"
}

variable "app_name" {
  description = "アプリ名（各種リソース名の接頭辞）"
  type        = string
  default     = "kessai"
}

variable "vpc_cidr" {
  description = "VPCのCIDR"
  type        = string
  default     = "10.30.0.0/16"
}

variable "az_count" {
  description = "AZ数（最低2で高可用性）"
  type        = number
  default     = 2
}

variable "container_port" {
  description = "アプリのListenポート"
  type        = number
  default     = 8080
}

variable "task_cpu" {
  description = "Fargateタスクの vCPU 単位（256=0.25vCPU、512=0.5vCPU、1024=1vCPU）"
  type        = number
  default     = 512
}

variable "task_memory" {
  description = "Fargateタスクのメモリ（MB）"
  type        = number
  default     = 1024
}

variable "desired_count" {
  description = "常時起動するタスク数"
  type        = number
  default     = 1
}

variable "db_instance_class" {
  description = "RDSインスタンスクラス"
  type        = string
  default     = "db.t4g.micro"
}

variable "db_allocated_storage" {
  description = "RDSストレージ（GB）"
  type        = number
  default     = 20
}

variable "container_image" {
  description = "実行するコンテナイメージ（例: <account>.dkr.ecr.<region>.amazonaws.com/kessai:latest）。空ならECRのlatest"
  type        = string
  default     = ""
}

variable "stripe_secret_key" {
  description = "Stripe Secret Key（Test/Live）。terraform apply時に指定"
  type        = string
  sensitive   = true
}

variable "stripe_publishable_key" {
  description = "Stripe Publishable Key（フロント公開用）"
  type        = string
  sensitive   = true
}

variable "stripe_webhook_secret" {
  description = "Stripe Webhook 署名検証シークレット（whsec_...）"
  type        = string
  sensitive   = true
}

variable "admin_email" {
  description = "初期adminのメールアドレス"
  type        = string
}

variable "session_signing_key" {
  description = "セッション署名キー（32バイト以上）"
  type        = string
  sensitive   = true
}

variable "enable_budget_mode" {
  description = "true=コスト最適化（NAT削除、ECSをpublicサブネット/public IP、Fargate Spot、RDSバックアップ0）"
  type        = bool
  default     = false
}
