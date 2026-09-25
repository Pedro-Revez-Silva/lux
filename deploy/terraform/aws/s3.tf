variable "blob_bucket_name" {
  description = "S3 bucket for lux blobs (snapshots, output, artifacts). Empty: a name is generated from `name` and the account/region."
  type        = string
  default     = ""
}

variable "backup_bucket_name" {
  description = "S3 bucket for Postgres backups. Empty: a name is generated from `name` and the account/region."
  type        = string
  default     = ""
}

variable "backup_retention_days" {
  description = "Days a Postgres backup is kept before S3 expires it."
  type        = number
  default     = 30
}

data "aws_caller_identity" "current" {}

locals {
  blob_bucket_name   = var.blob_bucket_name != "" ? var.blob_bucket_name : "${var.name}-blobs-${data.aws_caller_identity.current.account_id}-${var.region}"
  backup_bucket_name = var.backup_bucket_name != "" ? var.backup_bucket_name : "${var.name}-pg-backups-${data.aws_caller_identity.current.account_id}-${var.region}"
}

# Snapshots, output and artifacts (docs/operations.md "Where bytes live").
# luxd streams into and out of it with its instance role; no bucket policy
# needed beyond blocking public access.
resource "aws_s3_bucket" "blobs" {
  bucket = local.blob_bucket_name

  tags = merge(var.tags, { Name = local.blob_bucket_name })
}

resource "aws_s3_bucket_public_access_block" "blobs" {
  bucket = aws_s3_bucket.blobs.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "blobs" {
  bucket = aws_s3_bucket.blobs.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "aws:kms"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_versioning" "blobs" {
  bucket = aws_s3_bucket.blobs.id

  versioning_configuration {
    # Retention (docs/operations.md) already deletes finished Runs' blobs;
    # versioning here would only pile up noncurrent versions of state that
    # is meant to be deleted, not restored.
    status = "Disabled"
  }
}

# Only incomplete multipart uploads expire here: blob uploads go through
# the S3 transfer manager (multipart), and an upload luxd abandons (a
# restart, a runner gone mid-stream) leaves parts that are billed but
# invisible to listings. No object expiry and no versioning: luxd's
# reaper (internal/server/reaper.go) owns blob retention.
resource "aws_s3_bucket_lifecycle_configuration" "blobs" {
  bucket = aws_s3_bucket.blobs.id

  rule {
    id     = "abort-incomplete-multipart-uploads"
    status = "Enabled"

    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

# pg_dump -Fc, daily via the control host's systemd timer.
resource "aws_s3_bucket" "pg_backups" {
  bucket = local.backup_bucket_name

  tags = merge(var.tags, { Name = local.backup_bucket_name })
}

resource "aws_s3_bucket_public_access_block" "pg_backups" {
  bucket = aws_s3_bucket.pg_backups.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "pg_backups" {
  bucket = aws_s3_bucket.pg_backups.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "aws:kms"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_versioning" "pg_backups" {
  bucket = aws_s3_bucket.pg_backups.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "pg_backups" {
  bucket = aws_s3_bucket.pg_backups.id

  rule {
    id     = "expire-backups"
    status = "Enabled"

    filter {}

    expiration {
      days = var.backup_retention_days
    }

    noncurrent_version_expiration {
      noncurrent_days = var.backup_retention_days
    }
  }
  # The nightly dump is streamed to `aws s3 cp -`, which uploads multipart
  # once it passes the CLI's 8 MiB threshold.
  rule {
    id     = "abort-incomplete-multipart-uploads"
    status = "Enabled"

    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  # Once expire-backups has removed a backup's last noncurrent version,
  # only its delete marker is left. S3 refuses expired_object_delete_marker
  # in an expiration block that also sets days, hence its own rule.
  rule {
    id     = "remove-expired-delete-markers"
    status = "Enabled"

    filter {}

    expiration {
      expired_object_delete_marker = true
    }
  }
}

output "blob_bucket_name" {
  value = aws_s3_bucket.blobs.id
}

output "backup_bucket_name" {
  value = aws_s3_bucket.pg_backups.id
}
