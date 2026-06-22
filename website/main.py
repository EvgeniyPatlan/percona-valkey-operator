"""main.py — function macros for the mkdocs-macros plugin.

The `macros` plugin imports this module and calls define_env() at build time.
It registers helper functions usable in Markdown as {{ k8svalkeyjira("K8SVALKEY-1") }}
and {{ blob("deploy/cr.yaml") }}, mirroring the k8spxc-docs / k8spsmdb-docs main.py
helpers (arch 10 §5).

`variables.yml` values (release, *recommended, etc.) are injected separately via
macros.include_yaml; this module only adds *function* macros that derive links.
"""


def define_env(env):
    """Register function macros. `env.variables` holds the included variables.yml."""

    jira_base = "https://perconadev.atlassian.net/browse/"
    repo = "https://github.com/percona/percona-valkey-operator"

    @env.macro
    def k8svalkeyjira(issue: str) -> str:
        """Render a Jira issue id as a Markdown link, e.g. K8SVALKEY-123."""
        return f"[{issue}]({jira_base}{issue})"

    @env.macro
    def blob(path: str) -> str:
        """Link a repo file at the released tag v<release> (arch 10 §5.1 trap).

        The tag MUST exist or the link 404s — that is exactly why the
        verify-release-tag CI job gates `mike` publish on the tag's existence.
        """
        release = env.variables.get("release", "main")
        return f"{repo}/blob/v{release}/{path}"
