# TG-4a mutation run (ADR-0083 security core)

Each entry of `mutants.py` swaps one exact string in a guard of the Telegram security core; `mutate.py` applies one
mutant at a time to a **copy** of the tree, runs the Telegram, in-process, approver and classification tests, and
restores the file. A guard is load-bearing when its mutant is killed (a test fails).

    rsync -a --exclude .git --exclude bin --exclude web/node_modules --exclude site/node_modules ./ /path/to/copy/
    python3 docs/mutation/tg4a/mutate.py /path/to/copy docs/mutation/tg4a/mutants.py [mutant ids…]

The results and the equivalent mutants are listed in the pull request's threat notes.
