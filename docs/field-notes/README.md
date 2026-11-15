# Field notes

Notes from spotexec's first trial on a real project: Daintree's full development checks, run on AWS Spot on 14 and 15 November 2026. Daintree is an Electron app with about 114,000 Vitest tests, native addons and npm workspaces. The trial started on v0.3.0 and ended on what became v0.4.

These notes are not reference documentation. The guides in [docs/](../) say how spotexec works. These say what happened when it met a real project, what that taught, and where it fits.

| Note | What's in it |
| :--- | :--- |
| [The Daintree trial](daintree-trial.md) | What was run, everything that broke and why, how each problem was fixed, the final numbers and the final config. |
| [Tuning a real project](tuning-a-real-project.md) | What `spotexec init` doesn't do for you, as a checklist, and how to debug a runner when something goes wrong. |
| [Where it fits](where-it-fits.md) | Where spotexec makes a difference and where it doesn't, coding agents, costs, and what hasn't been tested yet. |
| [README suggestions](readme-suggestions.md) | Changes the README would benefit from, based on the trial. |
| [Video brief](video-brief.md) | The story, the numbers worth showing, a demo script, and the caveats to say out loud. |
