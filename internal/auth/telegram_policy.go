package auth

// TelegramPolicyChanged has every open stream re-check its authorization after a change of the Telegram policy that
// changes what links act as (link_sso_grants, ADR-0083 Q8). Telegram principals are built per request, so requests
// pick the change up anyway; streams would otherwise keep their principal until their next periodic re-check.
func (svc *Service) TelegramPolicyChanged() {
	svc.rev.signal(revocation{all: true})
}
