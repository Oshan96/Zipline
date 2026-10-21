# Security policy

Zipline handles sign-in cookies and passwords, talks to a browser extension and, when you switch it on, to your phone. If you find a way to get around its protections, please tell us privately so it can be fixed before it's widely known.

## Reporting a vulnerability

Use **Report a vulnerability** on the [Security tab](https://github.com/Oshan96/Zipline/security) of this repository. It opens a private report that only the maintainers can see. Please don't open a public issue for a security problem.

A good report says which version you used (Help → Diagnostics), what an attacker can do, and the steps to show it. You'll get an answer to say the report has arrived, and news as it's looked into and fixed.

## Supported versions

Fixes go into the next release, which every merge to `main` produces. Use the [latest release](https://github.com/Oshan96/Zipline/releases/latest).

## How Zipline protects you

The user guide's [Security](docs/user-guide.md#security) section describes the model: the window's one-time sign-in, the secret that programs on your computer prove themselves with, the browser extension's signed link, the phone page that works only on private networks after you allow it, and the vault that encrypts saved passwords. Its [Limitations](docs/user-guide.md#limitations) section lists the known gaps.
