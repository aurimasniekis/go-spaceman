terraform {
  required_version = ">= 1.11" # write-only arguments
  required_providers {
    spaceman = {
      source = "aurimasniekis/spaceman"
    }
  }
}

# endpoint and token default to SPACEMAN_ENDPOINT and SPACEMAN_TOKEN.
provider "spaceman" {
  endpoint = "https://spaceman.example.com"
}
