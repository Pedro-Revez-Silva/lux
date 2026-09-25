variable "name" {
  description = "Prefix for resource names and the Name tag (e.g. \"lux\", \"lux-staging\")."
  type        = string
  default     = "lux"
}

variable "region" {
  description = "AWS region to deploy into."
  type        = string
}

variable "tags" {
  description = "Tags applied to every resource this module creates."
  type        = map(string)
  default     = {}

  # luxd marks what it launches with lux:* tags and may terminate
  # instances carrying them (iam.tf): they must never come from here.
  validation {
    condition     = length([for k in keys(var.tags) : k if startswith(k, "lux:")]) == 0
    error_message = "tags must not contain lux:* keys: those mark instances luxd launched and may terminate."
  }
}

variable "vpc_cidr" {
  description = "CIDR for the VPC."
  type        = string
  default     = "10.60.0.0/16"
}

variable "az_count" {
  description = "Number of availability zones to spread public subnets across (2-3)."
  type        = number
  default     = 2

  validation {
    condition     = var.az_count >= 2 && var.az_count <= 3
    error_message = "az_count must be 2 or 3."
  }
}

variable "luxd_port" {
  description = "Port luxd listens on (LUX_LISTEN); runners and the console reach it here."
  type        = number
  default     = 7070
}

output "luxd_port" {
  value = var.luxd_port
}
