# GitHub, Gitea, Forgejo

## Repository webhooks

These platforms sign their webhooks, so no token is needed in the URL.

In the repository: **Settings, Webhooks, Add webhook**:

- URL: `https://stellwerk.example.org/in/forgejo`
- Content type: `application/json`
- Secret: the same value as `signature.secret`

```yaml
sources:
  forgejo:
    kind: generic
    signature: { preset: forgejo, secret: "${FORGEJO_SECRET}" }   # github, gitea, forgejo
    options: { labels: { repo: repository.full_name, event: action } }
routes:
  - name: releases
    match: { source: forgejo, fields: { release.tag_name: ".+" } }
    to: misc
```

GitLab sends a plain token in a header instead:

```yaml
  gitlab: { kind: generic, token: "${GITLAB_TOKEN}", token_header: X-Gitlab-Token }
```

## A step in a workflow

```yaml
# .github/workflows/deploy.yml (Forgejo Actions and Gitea Actions alike)
      - name: Tell stellwerk
        if: failure()
        run: |
          curl -fsS -H "Authorization: Bearer ${{ secrets.STELLWERK_TOKEN }}" \
            -d "{\"title\": \"Deploy failed\", \"message\": \"${{ github.repository }} run ${{ github.run_id }}\"}" \
            https://stellwerk.example.org/in/ci
```
