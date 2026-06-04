config {
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

# Deeper, provider-aware static analysis. Run `tflint --init` once to download
# the plugin, then `tflint`. Requires network access to github.com for the
# initial install.
plugin "google" {
  enabled = true
  version = "0.34.0"
  source  = "github.com/terraform-linters/tflint-ruleset-google"
}
