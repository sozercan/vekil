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
var one = 1 //nolint:unused
var two = 2 //nolint:unused // explanatory prose
// end'''
        self.assertEqual(self.texts('main.go', source), ['// inline', '/* block', '* body', '*/', '// explanatory prose', '// end'])
        self.assertEqual(self.texts('go.mod', 'require example.com/a v1.0.0 // indirect\n// text'), ['// text'])

    def test_go_cgo_preamble_comment_groups(self):
        source = '''package sample

// detached prose

// #cgo darwin CFLAGS: -fobjc-arc
/* typedef int custom_int; */
// #include "bridge.h"
import "C"

var text = `
// #include "literal.h"
import "C"
`
// ordinary prose
var count = 0
'''
        self.assertEqual(self.texts('main.go', source), ['// detached prose', '// ordinary prose'])
        self.assertEqual(self.texts('main.go', 'package p // trailing\nimport "C"\n'), ['// trailing'])
        self.assertEqual(self.texts('main.go', 'package p\n/* detached */\n\nimport "C"\n'), ['/* detached */'])

    def test_go_cgo_grouped_imports(self):
        source = '''package sample

// ordinary import documentation
import (
    // #include "bridge.h"
    "C"
    "fmt"
)
'''
        self.assertEqual(self.texts('main.go', source), ['// ordinary import documentation'])
        self.assertEqual(self.texts('main.go', 'package p\n// #include "bridge.h"\nimport ("C")\n'), [])

    def test_c_preprocessor_comments(self):
        source = '#include <example.h> // note\n#define VALUE 1 /* explanation */\n'
        self.assertEqual(self.texts('bridge.h', source), ['// note', '/* explanation */'])

    def test_make_comments(self):
        source = r'''VAR := value# assignment
ESCAPED := abc\#literal
SUB := $(subst #,Z,a#b)
export PATH
include other.mk # include
target: ; echo "# literal" # inline recipe
recipe:
	printf '%s' \
	  'quoted # data' # recipe
'''
        self.assertEqual(self.texts('Makefile', source), ['# assignment', '# include', '# inline recipe', '# recipe'])
        self.assertEqual(self.texts('build/shared.mk', 'VAR := value# assignment'), ['# assignment'])

    def test_make_define_boundaries(self):
        source = '''define := variable named define # assignment
override define COMMAND # opening header
    # literal body
define NESTED # literal nested header
    # literal nested body
endef
endef # closing header
VAR := value # after define
'''
        for path in ('Makefile', 'build/shared.mk'):
            with self.subTest(path=path):
                self.assertEqual(self.texts(path, source),
                                 ['# assignment', '# opening header', '# closing header', '# after define'])

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

    def test_expanding_bash_heredoc_with_argument_count(self):
        source = r'''cat <<EOF_PROXY
#!/usr/bin/env bash
while [[ \$# -gt 0 ]]; do # real comment
  shift
done
EOF_PROXY
'''
        result = list(comment_lines('scripts/run.sh', source))
        self.assertEqual([(line.number, line.text, line.inline) for line in result], [(3, '# real comment', True)])

    def test_jq_filters_and_shell_data(self):
        source = r'''echo jq '# shell data'
cat <<'DATA'
jq '# heredoc data'
DATA
if jq -e --arg name "${name}" \
  '.data | map(select(.name == "# literal"))
   # filter documentation
   | length # trailing filter text
  ' input.json; then
  echo ok
fi
'''
        result = list(comment_lines('scripts/run.sh', source))
        self.assertEqual([(line.number, line.text, line.inline) for line in result],
                         [(7, '# filter documentation', False), (8, '# trailing filter text', True)])
        self.assertEqual(self.texts('program.jq', '"# literal" # actual comment\n'), ['# actual comment'])

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
            strict = lint()
            self.assertEqual(strict.returncode, 1)
            self.assertIn('main.go:2:1: comment: // legacy', strict.stderr)
            self.assertEqual(lint('--base', 'HEAD').returncode, 0)
            source.write_text('package main\n', encoding='utf-8')
            self.assertEqual(lint().returncode, 0)
            commit()

            source.write_text('package main\n// legacy\n', encoding='utf-8')
            new = root / 'new.go'
            new.write_text('package main\n// another\n', encoding='utf-8')
            result = lint()
            self.assertEqual(result.returncode, 1)
            self.assertIn('main.go:2:1: comment: // legacy', result.stderr)
            self.assertIn('new.go:2:1: comment: // another', result.stderr)
            ratchet = lint('--base', 'HEAD')
            self.assertEqual(ratchet.returncode, 1)
            self.assertIn('main.go:2:1: new comment: // legacy', ratchet.stderr)
            new.unlink()
            self.assertEqual(lint('--base', 'HEAD^').returncode, 0)


if __name__ == '__main__':
    unittest.main()
