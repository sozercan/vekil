from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest


LINTER = Path(__file__).resolve().parents[1] / 'comment-lint.py'
MODULE = runpy.run_path(str(LINTER))
comment_lines = MODULE['comment_lines']
additions = MODULE['additions']


class CommentLintTest(unittest.TestCase):
    def texts(self, path, source):
        return [line.text for line in comment_lines(path, source)]

    def test_go_strings_and_directives(self):
        source = '''//go:build darwin

package sample

/*
#include <stdio.h>
*/
import "C"

//go:embed asset
var text string
var url = "https://example.com"
var raw = `// literal`
var count = 0 // inline
/* block
 * body
 */
// end'''
        self.assertEqual(self.texts('main.go', source), ['// inline', '/* block', '* body', '*/', '// end'])
        self.assertEqual(self.texts('go.mod', 'require example.com/a v1.0.0 // indirect\n// text'), ['// text'])

    def test_embedded_javascript_css_and_html(self):
        source = '''<!-- markup -->
<style>/* style */</style>
<script>
const url = "https://example.com";
const slash = /https?:\\/\\//;
// script
</script>'''
        self.assertEqual(self.texts('dashboard.html', source), ['<!-- markup -->', '/* style */', '// script'])
        self.assertEqual(self.texts('proxy/dashboard/uPlot.min.js', '// upstream banner'), [])

    def test_shell_heredoc_and_docker(self):
        source = '''#!/usr/bin/env bash
echo "# literal" # shell
test "${NAME#prefix}" = tail
cat <<'DATA'
# fixture
DATA
python3 - <<'PY'
print("# literal") # python
PY
echo anything # eof'''
        self.assertEqual(self.texts('scripts/run.sh', source), ['# shell', '# python', '# eof'])
        dockerfile = '''# syntax=docker/dockerfile:1
  # Docker
FROM scratch
RUN echo "# literal" # shell
CMD ["echo", "# literal"]
COPY a b # not a Docker comment'''
        self.assertEqual(self.texts('Dockerfile.dev', dockerfile), ['# Docker', '# shell'])

    def test_workflow_embedded_comments(self):
        source = '''jobs:
  lint:
    steps:
      - run: | # YAML
          echo "# literal" # shell
          # second line
      - run: "echo anything # quoted"
      - run: >-
          echo anything
          # folded
'''
        result = list(comment_lines('.github/workflows/ci.yaml', source))
        self.assertEqual(
            [(line.number, line.text, line.inline) for line in result],
            [(4, '# YAML', True), (5, '# shell', True), (6, '# second line', False),
             (7, '# quoted', True), (10, '# folded', False)],
        )
        self.assertEqual(self.texts('ordinary.yaml', source), ['# YAML'])

    def test_ratchet_is_a_multiset_not_a_line_budget(self):
        before = '// existing\npackage test\n// removable\n'
        self.assertEqual(list(additions('main.go', before, 'package test\n// existing\n')), [])
        after = '// existing\npackage test\n// existing\n// replacement\n'
        self.assertEqual([line.text for line in additions('main.go', before, after)], ['// existing', '// replacement'])

    def test_cli_counts_untracked_and_prevents_reintroduction(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / 'main.go'

            def git(*args):
                subprocess.run(['git', *args], cwd=root, check=True, capture_output=True)

            def commit():
                git('add', 'main.go')
                git('-c', 'user.name=Lint Test', '-c', 'user.email=lint@example.com',
                    'commit', '--no-gpg-sign', '-m', 'Test base')

            def lint(*args):
                return subprocess.run([sys.executable, str(LINTER), *args], cwd=root, capture_output=True, text=True)

            git('init')
            source.write_text('package main\n// legacy\n', encoding='utf-8')
            commit()
            source.write_text('package main\n', encoding='utf-8')
            self.assertEqual(lint().returncode, 0)
            commit()

            source.write_text('package main\n// legacy\n', encoding='utf-8')
            new = root / 'new.go'
            new.write_text('package main\n// another\n', encoding='utf-8')
            result = lint()
            self.assertEqual(result.returncode, 1)
            self.assertIn('main.go:2:1: new comment: // legacy', result.stderr)
            self.assertIn('new.go:2:1: new comment: // another', result.stderr)
            new.unlink()
            self.assertEqual(lint('--base', 'HEAD^').returncode, 0)


if __name__ == '__main__':
    unittest.main()
