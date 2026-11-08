# Your own MCP client

SpaceMan serves its administrative API as MCP tools at **`/api/v1/mcp`**, so an MCP client you run yourself (an editor,
a desktop assistant, a terminal agent) can work with SpaceMan directly. The client signs in with one of your
[API tokens](../operators/access#api-tokens) and can do exactly what that token can do on the API, with the same
exclusions and approvals that apply to agents' SpaceMan MCP tools (ADR-0063).

## What you need

- The address of your SpaceMan installation, for example `https://spaceman.example.com`.
- An API token (`spaceman_at_…`) from the **API tokens** page. Its grants decide which tools the client gets, so give
  the client its own token with only the grants it needs: `viewer` on one space for a read-only assistant, `operator`
  to also create tasks, launch runs and send messages.
- A client that can talk to a remote MCP server over **streamable HTTP** and send an `Authorization` header. SpaceMan
  does not offer OAuth for this endpoint.

Keep the token out of files you commit: most clients can read it from an environment variable.

## Claude Code

Checked with Claude Code 2.1.317 against a local test installation: `claude mcp list` reports the server as connected
and its tools are listed.

```sh
export SPACEMAN_TOKEN=spaceman_at_...   # your token
claude mcp add --transport http spaceman https://spaceman.example.com/api/v1/mcp \
  --header "Authorization: Bearer $SPACEMAN_TOKEN"
```

The shell expands `$SPACEMAN_TOKEN` when you run the command, so the token is stored in Claude Code's own
configuration; `--scope user` or `--scope local` keeps it out of the project's `.mcp.json`.

## Other clients

Configure a remote (streamable HTTP) MCP server with:

| Setting | Value |
|---|---|
| URL | `https://spaceman.example.com/api/v1/mcp` |
| Header | `Authorization: Bearer spaceman_at_...` |

The server is stateless: every request carries the token, there is no MCP session to keep, and only `POST` is
served. To check the address and token by hand:

```sh
curl -sS https://spaceman.example.com/api/v1/mcp \
  -H "Authorization: Bearer $SPACEMAN_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}'
```

## Which tools the client gets

One tool per API operation, named after the operation (`list_spaces`, `create_task`, `launch_run`, …), limited to
what the token's grants can reach. Results are JSON objects with the HTTP `status`, the resource version (`etag`) and
the response `body`; an API error comes back as a tool error with the same problem the API returns.

- When the token is granted exactly one space, `space_id` may be left out and means that space.
- Never offered: anything that needs you at the keyboard (sign-in, your own account, step-up operations such as
  creating tokens or users), answering human actions and deciding approvals, terminal takeover and input, operations
  that return a one-time secret, and SpaceMan MCP management.
- Not offered here, though the API has them: terminal recordings and transcripts, and dangerous operations that act on
  no particular space (accounts, Git SSH keys, MCP servers, harness profiles, role templates, publishing,
  installation-wide human policies, node changes). Use the web UI, the CLI or the API for those.

## Dangerous tools need someone else's approval

Deletes, secrets, space roles, diagnostic launches and the other dangerous operations never run
straight away. The first call creates an [enforced approval](./approvals) in the space and returns a tool error that
says so; the request appears under **Needs you** for the people who can decide approvals there. Once it is approved,
the client calls the tool again with **exactly the same arguments**, and it runs once.

**You can't approve your own client's request**, from any session or token of yours: another person has to. You can
still deny or cancel it, for example when your client asked for something you didn't mean. If you are the only
administrator, run dangerous operations from the web UI or the CLI instead.

The root token can currently approve these requests. Whether it should is under review (SR-MCP-01).

Read and write tools run at once, as the same token's API calls would.

## Limits and refusals

- Browsers can't use the endpoint: requests with an `Origin` or `Sec-Fetch-*` header are refused, and no CORS headers
  are sent. Session cookies, the root token and SpaceMan's internal tokens are refused too.
- Each token may make 5 requests per second, with bursts of 50; more get `429`. Each request carries one JSON-RPC
  message: batches (and protocol versions older than 2025-06-18, which allow them) are refused with `400`. Request bodies are capped by
  `http.max_body_bytes`, and repeated failed sign-ins from one address are throttled as for the API.
- Revoking the token, or the user losing grants or being disabled, stops the client on its next request.

## Audit

Everything the client does is recorded as you, `via api_token:<token id>`, with `channel: mcp_endpoint`. Each tool
call is also recorded as `mcp_endpoint.tool_called` with the tool, its class and the outcome, never its arguments or
results. Filter the audit log by that action to see what a client did.
