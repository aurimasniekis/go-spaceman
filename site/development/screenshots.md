# Screenshots

Every screenshot on this site is the real SpaceMan UI, captured by Playwright from a throwaway local stack seeded with
fictional data: example.com people, invented spaces and the stub harness. Nothing shown is a mock-up.

```sh
scripts/screenshots.sh            # writes site/public/screenshots/<name>-dark.png and -light.png
```

The script starts a fresh `scripts/dev-stack.sh` stack, seeds it through the API (`web/e2e/screenshots.spec.ts`),
captures every shot in the Night Owl and Light Owl themes at 1440×900, and destroys the stack and its credentials.
Review every image before committing it. `pnpm build` in `site/` fails when a page shows a screenshot that is missing or
not listed in the inventory below.

<!--@include: ../../docs/SCREENSHOTS.md-->
