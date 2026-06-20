# Invalid Results

Run artifacts here were collected but are NOT valid benchmark measurements.

| Run ID | Reason | Valid replacement |
|--------|--------|-------------------|
| a4601b32 | hooks-OFF run — hooks_toggle.sh never fired, hooks remained ON. Result is identical to hooks-ON (11.4 RPS, 237ms p50). | bd89b7da |

> Note: the `a4601b32` artifact files were not present in this repo when this
> directory was created (they were still on the AWS runner). This README records
> the intent and blocks the run ID from being treated as valid; if the files are
> later pulled down, move any `*a4601b32*` files from `results/` into this folder.
