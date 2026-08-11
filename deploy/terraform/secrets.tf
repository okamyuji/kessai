# シークレットは AWS Secrets Manager に集約し、ECSタスク実行ロールから取得します。
resource "aws_secretsmanager_secret" "app" {
  name = "${var.app_name}-${var.environment}-secrets"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "app" {
  secret_id = aws_secretsmanager_secret.app.id
  secret_string = jsonencode({
    DATABASE_URL          = local.database_url
    STRIPE_SECRET_KEY     = var.stripe_secret_key
    STRIPE_PUBLISHABLE_KEY = var.stripe_publishable_key
    STRIPE_WEBHOOK_SECRET = var.stripe_webhook_secret
    SESSION_SIGNING_KEY   = var.session_signing_key
    ADMIN_EMAIL           = var.admin_email
  })
}
