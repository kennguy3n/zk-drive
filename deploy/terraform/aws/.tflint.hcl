config {
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

# Deeper, provider-aware static analysis (invalid instance types, deprecated
# attributes, etc.). Run `tflint --init` once to download the plugin, then
# `tflint`. Requires network access to github.com for the initial install.
plugin "aws" {
  enabled = true
  version = "0.42.0"
  source  = "github.com/terraform-linters/tflint-ruleset-aws"
}
