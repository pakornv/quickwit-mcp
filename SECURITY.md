# Security

Please report suspected vulnerabilities privately using the repository's GitHub security advisory feature. Do not include real logs, credentials, personal data, or private endpoints in a report. Include the affected version, a synthetic reproduction, and the potential impact.

The server is read-only at the tool level, not an authorization boundary. It does not authenticate to Quickwit, restrict indexes, or redact results. Review the security and privacy notes in the README before connecting it to a cluster or an external model provider.
