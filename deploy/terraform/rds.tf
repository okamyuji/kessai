# RDS PostgreSQL 18（ADR-0003に準拠）
resource "aws_db_subnet_group" "db" {
  name       = "${var.app_name}-${var.environment}-dbsg"
  # budget: RDSはprivateサブネット内で完結（インターネットに出ない）ためNAT無しでも問題ない
  subnet_ids = aws_subnet.private[*].id
}

resource "random_password" "db" {
  length  = 32
  special = false
}

resource "aws_db_instance" "postgres" {
  identifier                  = "${var.app_name}-${var.environment}"
  engine                      = "postgres"
  engine_version              = "18.4"
  instance_class              = var.db_instance_class
  allocated_storage           = var.db_allocated_storage
  storage_type                = "gp3"
  storage_encrypted           = true
  db_name                     = "kessai"
  username                    = "kessai"
  password                    = random_password.db.result
  db_subnet_group_name        = aws_db_subnet_group.db.name
  vpc_security_group_ids      = [aws_security_group.db.id]
  publicly_accessible         = false
  skip_final_snapshot         = true
  deletion_protection         = false
  auto_minor_version_upgrade  = true
  performance_insights_enabled = false
  apply_immediately           = true
  backup_retention_period     = var.enable_budget_mode ? 0 : 1
}

# アプリが読む DATABASE_URL を Secrets Manager 経由で注入
locals {
  database_url = "postgres://${aws_db_instance.postgres.username}:${random_password.db.result}@${aws_db_instance.postgres.address}:${aws_db_instance.postgres.port}/${aws_db_instance.postgres.db_name}?sslmode=require"
}
