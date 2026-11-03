package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// repositoriesHCL is a space with an API repository (optional settings in api) and a manual notes repository at notes.
func repositoriesHCL(slug, nodeID, api, notesPath string, withRepos bool) string {
	s := spaceHCL(slug, "Repositories", "", nodeID)
	if !withRepos {
		return s
	}
	return s + fmt.Sprintf(`
resource "spaceman_space_repository" "api" {
  space_id = spaceman_space.s.id
  name     = "api"
  path     = "repos/api"
  url      = "https://git.example.com/acme/api.git"
%s
}

resource "spaceman_space_repository" "notes" {
  space_id = spaceman_space.s.id
  name     = "notes"
  path     = %q
}
`, api, notesPath)
}

// TestAccSpaceRepository (SM-API-004, ADR-0044): create with server defaults, update in place, import by name and ID,
// drift of a configured field and none for an unconfigured one, replacement on a path change, and removal.
func TestAccSpaceRepository(t *testing.T) {
	testAccPreCheck(t)
	slug := "tf-repo-" + suffix()
	node := acc.nodeA.ID
	var spaceID, apiID, notesID string
	edit := func(id string, f func(*client.SpaceRepository, *client.SpaceRepositoryInput)) {
		t.Helper()
		ctx := context.Background()
		cur, err := acc.client.GetSpaceRepository(ctx, spaceID, id)
		if err != nil {
			t.Fatal(err)
		}
		in := client.SpaceRepositoryInput{Description: cur.Description, URL: cur.URL, DefaultBranch: cur.DefaultBranch, Role: cur.Role, ClonePolicy: cur.ClonePolicy}
		f(cur, &in)
		if _, err := acc.client.UpdateSpaceRepository(ctx, spaceID, id, cur.Version, in); err != nil {
			t.Fatal(err)
		}
	}
	updated := `  default_branch = "trunk"
  clone_policy   = "on_demand"
  description    = "Settlement API"`
	accSteps(t,
		resource.TestStep{
			Config: repositoriesHCL(slug, node, "", "notes", true),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_space_repository.api", "clone_policy", "auto"),
				resource.TestCheckResourceAttr("spaceman_space_repository.api", "default_branch", "main"),
				resource.TestCheckResourceAttr("spaceman_space_repository.api", "role", "code"),
				resource.TestCheckResourceAttr("spaceman_space_repository.notes", "clone_policy", "manual"),
				resource.TestCheckResourceAttr("spaceman_space_repository.notes", "url", ""),
				func(s *terraform.State) error {
					spaceID, apiID, notesID = stateAttr(s, "spaceman_space.s", "id"), stateAttr(s, "spaceman_space_repository.api", "id"),
						stateAttr(s, "spaceman_space_repository.notes", "id")
					return nil
				},
			),
		},
		resource.TestStep{Config: repositoriesHCL(slug, node, "", "notes", true), PlanOnly: true},
		resource.TestStep{
			Config: repositoriesHCL(slug, node, updated, "notes", true),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("spaceman_space_repository.api", plancheck.ResourceActionUpdate)}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_space_repository.api", "default_branch", "trunk"),
				resource.TestCheckResourceAttr("spaceman_space_repository.api", "clone_policy", "on_demand"),
				resource.TestCheckResourceAttr("spaceman_space_repository.api", "description", "Settlement API"),
				apiCheck("spaceman_space_repository.api", func(ctx context.Context, c *client.Client, id string, _ *terraform.State) error {
					r, err := c.GetSpaceRepository(ctx, spaceID, id)
					if err != nil {
						return err
					}
					if r.Version != 2 || r.Path != "repos/api" || r.ID != apiID {
						return fmt.Errorf("after update: %+v", r)
					}
					return nil
				}),
			),
		},
		resource.TestStep{Config: repositoriesHCL(slug, node, updated, "notes", true), PlanOnly: true},
		resource.TestStep{ResourceName: "spaceman_space_repository.api", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) { return stateAttr(s, "spaceman_space.s", "id") + "/api", nil }},
		resource.TestStep{ResourceName: "spaceman_space_repository.notes", ImportState: true, ImportStateVerify: true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return stateAttr(s, "spaceman_space.s", "id") + "/" + stateAttr(s, "spaceman_space_repository.notes", "id"), nil
			}},
		// An unconfigured field edited outside Terraform is not drift; a configured one is, and the next apply restores it.
		resource.TestStep{
			PreConfig: func() {
				edit(notesID, func(_ *client.SpaceRepository, in *client.SpaceRepositoryInput) { in.Description = "from the UI" })
			},
			Config: repositoriesHCL(slug, node, updated, "notes", true), PlanOnly: true,
		},
		resource.TestStep{
			PreConfig: func() {
				edit(apiID, func(_ *client.SpaceRepository, in *client.SpaceRepositoryInput) {
					in.URL = "https://git.example.com/acme/api-fork.git"
				})
			},
			Config: repositoriesHCL(slug, node, updated, "notes", true), PlanOnly: true, ExpectNonEmptyPlan: true,
		},
		resource.TestStep{
			Config: repositoriesHCL(slug, node, updated, "notes", true),
			Check: apiCheck("spaceman_space_repository.api", func(ctx context.Context, c *client.Client, id string, _ *terraform.State) error {
				r, err := c.GetSpaceRepository(ctx, spaceID, id)
				if err != nil {
					return err
				}
				n, err := c.GetSpaceRepository(ctx, spaceID, notesID)
				if err != nil {
					return err
				}
				if r.URL != "https://git.example.com/acme/api.git" || n.Description != "from the UI" {
					return fmt.Errorf("url %q, notes description %q after correcting drift", r.URL, n.Description)
				}
				return nil
			}),
		},
		// The folder is fixed: a new path replaces the repository.
		resource.TestStep{
			Config: repositoriesHCL(slug, node, updated, "docs/notes", true),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("spaceman_space_repository.notes", plancheck.ResourceActionDestroyBeforeCreate)}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("spaceman_space_repository.notes", "path", "docs/notes"),
				func(s *terraform.State) error {
					if stateAttr(s, "spaceman_space_repository.notes", "id") == notesID {
						return fmt.Errorf("notes was not replaced")
					}
					return nil
				},
			),
		},
		// Destroying a repository removes it from the space's list.
		resource.TestStep{
			Config: repositoriesHCL(slug, node, "", "", false),
			Check: func(*terraform.State) error {
				if _, err := acc.client.GetSpaceRepository(context.Background(), spaceID, apiID); !client.IsNotFound(err) {
					return fmt.Errorf("repository after destroy: %w", err)
				}
				return nil
			},
		},
	)
}
