resource "aws_ecs_cluster" "main" {
  name = "${var.app_name}-${var.environment}"
}

# Budget: FARGATE_SPOT を capacity provider として有効化
resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = var.enable_budget_mode ? ["FARGATE_SPOT", "FARGATE"] : ["FARGATE"]

  default_capacity_provider_strategy {
    capacity_provider = var.enable_budget_mode ? "FARGATE_SPOT" : "FARGATE"
    weight            = 1
    base              = 1
  }
}

# 実行ロール（ECRからPull、CloudWatch Logs書込、Secrets Manager取得）
data "aws_iam_policy_document" "task_execution_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "task_execution" {
  name               = "${var.app_name}-${var.environment}-task-exec"
  assume_role_policy = data.aws_iam_policy_document.task_execution_assume.json
}

resource "aws_iam_role_policy_attachment" "task_execution_default" {
  role       = aws_iam_role.task_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "task_execution_secrets" {
  role = aws_iam_role.task_execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["secretsmanager:GetSecretValue"]
      Resource = [aws_secretsmanager_secret.app.arn]
    }]
  })
}

# タスクロール（アプリ実行中に必要な権限。現状は空、将来S3等追加）
resource "aws_iam_role" "task" {
  name               = "${var.app_name}-${var.environment}-task"
  assume_role_policy = data.aws_iam_policy_document.task_execution_assume.json
}

# CloudWatch Logs
resource "aws_cloudwatch_log_group" "app" {
  name              = "/ecs/${var.app_name}-${var.environment}"
  retention_in_days = 14
}

locals {
  image_uri = var.container_image != "" ? var.container_image : "${aws_ecr_repository.app.repository_url}:latest"

  # Secrets Manager から各キーを個別に環境変数へ写す
  secrets = [
    { name = "DATABASE_URL",           valueFrom = "${aws_secretsmanager_secret.app.arn}:DATABASE_URL::" },
    { name = "STRIPE_SECRET_KEY",      valueFrom = "${aws_secretsmanager_secret.app.arn}:STRIPE_SECRET_KEY::" },
    { name = "STRIPE_PUBLISHABLE_KEY", valueFrom = "${aws_secretsmanager_secret.app.arn}:STRIPE_PUBLISHABLE_KEY::" },
    { name = "STRIPE_WEBHOOK_SECRET",  valueFrom = "${aws_secretsmanager_secret.app.arn}:STRIPE_WEBHOOK_SECRET::" },
    { name = "SESSION_SIGNING_KEY",    valueFrom = "${aws_secretsmanager_secret.app.arn}:SESSION_SIGNING_KEY::" },
    { name = "ADMIN_EMAIL",            valueFrom = "${aws_secretsmanager_secret.app.arn}:ADMIN_EMAIL::" },
  ]
}

resource "aws_ecs_task_definition" "app" {
  family                   = "${var.app_name}-${var.environment}"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = var.task_cpu
  memory                   = var.task_memory
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn
  # ARM64 (AWS Graviton) を採用してコスト削減 + ローカルARM Macでのネイティブビルドを可能に
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([{
    name      = var.app_name
    image     = local.image_uri
    essential = true
    portMappings = [{ containerPort = var.container_port, protocol = "tcp" }]
    environment = [
      { name = "HTTP_ADDR",   value = "0.0.0.0:${var.container_port}" },
      { name = "LOG_LEVEL",   value = "info" },
      { name = "CAPTURE_MODE", value = "manual" },
      { name = "CHECKOUT_EXPIRY_MINUTES", value = "60" },
      { name = "AUTH_EXPIRY_DAYS", value = "21" },
    ]
    secrets = local.secrets
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.app.name
        awslogs-region        = var.aws_region
        awslogs-stream-prefix = var.app_name
      }
    }
  }])
}

resource "aws_ecs_service" "app" {
  name            = "${var.app_name}-${var.environment}"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.app.arn
  desired_count   = var.desired_count

  # Budget時はFARGATE_SPOT、標準時はFARGATE
  capacity_provider_strategy {
    capacity_provider = var.enable_budget_mode ? "FARGATE_SPOT" : "FARGATE"
    weight            = 1
    base              = 1
  }

  network_configuration {
    # Budget時はpublicサブネット+public IPでNAT不要にする
    subnets          = var.enable_budget_mode ? aws_subnet.public[*].id : aws_subnet.private[*].id
    security_groups  = [aws_security_group.app.id]
    assign_public_ip = var.enable_budget_mode
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.app.arn
    container_name   = var.app_name
    container_port   = var.container_port
  }

  depends_on = [aws_lb_listener.http, aws_ecs_cluster_capacity_providers.main]
}
