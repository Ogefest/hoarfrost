<!-- What does this change and why? Link the issue it closes, if any. -->

**Label** the pull request so it lands in the right section of the release
notes: `enhancement`, `bug`, `documentation`, plus `migration` when it adds
a database migration and `breaking` when users must act on upgrade (say
what in the description). `skip-changelog` leaves it out.

- [ ] `make test lint` pass (with `HOARFROST_TEST_DATABASE` set)
- [ ] docs updated (README, docs/, `.env.example`) if behaviour or settings changed
