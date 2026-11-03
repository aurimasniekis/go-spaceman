data "spaceman_nodes" "all" {}

output "connected_nodes" {
  value = [for n in data.spaceman_nodes.all.nodes : n.name if n.connected]
}
