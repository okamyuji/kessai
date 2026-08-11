output "alb_dns" {
  description = "ALBのDNS名（決済ページのエンドポイント）"
  value       = aws_lb.app.dns_name
}

output "ecr_repository_url" {
  description = "ECRリポジトリURL（docker push対象）"
  value       = aws_ecr_repository.app.repository_url
}

output "rds_endpoint" {
  description = "RDSエンドポイント（診断用途）"
  value       = aws_db_instance.postgres.address
}

output "secret_arn" {
  description = "Secrets Manager ARN"
  value       = aws_secretsmanager_secret.app.arn
}

output "cluster_name" {
  description = "ECSクラスタ名"
  value       = aws_ecs_cluster.main.name
}
