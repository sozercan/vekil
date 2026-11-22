import argparse
from bisect import bisect_right
from collections import Counter
from dataclasses import dataclass
from functools import cache
import os
from pathlib import Path
import re
import subprocess
import sys

from pygments.lexers import get_lexer_by_name
from pygments.token import Comment, String, Text
import yaml
from yaml.nodes import MappingNode, ScalarNode, SequenceNode


LEXERS = {
    '.css': 'css', '.go': 'go', '.h': 'c', '.html': 'html', '.js': 'javascript',
    '.jq': 'jq', '.json': 'json', '.m': 'objective-c', '.mk': 'make', '.py': 'python', '.sh': 'bash',
    '.toml': 'toml', '.yaml': 'yaml', '.yml': 'yaml',
}
NAMED_LEXERS = {'.gitignore': 'text', 'go.mod': 'go', 'GNUmakefile': 'make', 'Makefile': 'make', 'makefile': 'make'}
VENDORED = {'proxy/dashboard/uPlot.min.css', 'proxy/dashboard/uPlot.min.js'}
GO_DIRECTIVE = re.compile(
    r'//(?:go:(?:build|debug|embed|generate|linkname|wasmimport|wasmexport)\s+\S.*'
    r'|go:(?:noescape|nosplit|noinline|norace|notinheap|uintptrescapes)'
    r'| \+build\s+\S.*|line \S+:\d+(?::\d+)?|export \w+)'
)
GO_NOLINT = re.compile(r'//nolint:[\w-]+(?:,[\w-]+)*')
HEREDOC = re.compile(r'''<<-?[ \t]*(?P<quote>['"]?)(?P<label>\w+)(?P=quote)''')
JQ_TOKEN = re.compile(r'"(?:\\[\s\S]|[^"\\])*"|#[^\r\n]*')
SHELL_SPACE = r'(?:[ \t]|\\\n)+'
SHELL_WORD = r'''(?:"(?:\\[\s\S]|[^"\\])*"|'[^']*'|[^\s'";&|]+)'''
JQ_FILTER = re.compile(
    rf'''(?:^|(?<=\$\()|(?<=[;&|]))[ \t]*(?:(?:if|elif|then|else|!){SHELL_SPACE})?'''
    rf'''(?P<command>jq){SHELL_SPACE}(?:(?:-[A-Za-z]+|--(?:raw-output|exit-status|'''
    rf'''compact-output|slurp|null-input|sort-keys)|--arg(?:json)?{SHELL_SPACE}\w+'''
    rf'''{SHELL_SPACE}{SHELL_WORD}){SHELL_SPACE})*'(?P<program>[^']*)' ''',
    re.M | re.X,
)
MAKE_LINE = re.compile(r'(?:[^\n]*\\\n)*[^\n]+|(?:[^\n]*\\\n)+')
MAKE_DIRECTIVE = re.compile(r'(?:(?:override|export|private)\s+)*(define|endef)\b')
MAKE_ASSIGNMENT = re.compile(r'(?:[+?!]|:{1,3})?=')
DOCKER_SHELL = re.compile(
    r'^[ \t]*(?:ONBUILD[ \t]+)?(?:RUN|CMD|ENTRYPOINT|HEALTHCHECK[ \t]+CMD)[ \t]+'
    r'((?:[^\n]*\\\n)*[^\n]*)', re.I | re.M
)


@dataclass(frozen=True)
class CommentLine:
    number: int
    column: int
    text: str
    inline: bool


def source_language(path):
    name = Path(path).name
    if path in VENDORED:
        return None
    if name == 'Dockerfile' or name.startswith('Dockerfile.'):
        return 'docker'
    return NAMED_LEXERS.get(name) or LEXERS.get(Path(path).suffix)


@cache
def lexer(language):
    return get_lexer_by_name(language)


def go_directive(source, start, text):
    if GO_DIRECTIVE.fullmatch(text):
        line_start = source.rfind('\n', 0, start) + 1
        return not source[line_start:start].strip()
    return text.startswith('/*') and re.match(r'\s*import\s+"C"', source[start + len(text):])


def heredoc_language(label, body):
    first = body.lstrip().split('\n', 1)[0]
    if first.startswith('#!'):
        if re.search(r'\bpython(?:[23](?:\.\d+)?)?\b', first):
            return 'python'
        if re.search(r'\b(?:ba)?sh\b', first):
            return 'bash'
    if re.fullmatch(r'PY(?:THON|_\w+)?', label):
        return 'python'
    if re.fullmatch(r'SH(?:_\w+)?', label):
        return 'bash'
    return None


def make_comments(source):
    define_depth = 0
    for logical in MAKE_LINE.finditer(source):
        line = logical.group()
        if line.startswith('\t') and not define_depth:
            yield from ((logical.start() + offset, text) for offset, text in lex_comments('bash', line))
            continue
        directive = MAKE_DIRECTIVE.match(line.lstrip(' '))
        if directive and directive[1] == 'endef' and define_depth:
            define_depth -= 1
        if define_depth or (directive and directive[1] == 'define'):
            define_depth += int(bool(directive and directive[1] == 'define'))
            continue
        references = []
        target = assignment = False
        for token in re.finditer(r'\\[\s\S]|\$[$({]|[(){}#;]|(?:[+?!]|:{1,3})?=|:', line):
            text = token.group()
            if text in ('$(', '${'):
                references.append(')' if text == '$(' else '}')
            elif references:
                if text == references[-1]:
                    references.pop()
                elif (text, references[-1]) in {('(', ')'), ('{', '}')}:
                    references.append(references[-1])
            elif text == '#':
                yield logical.start() + token.start(), line[token.start():].rstrip('\n')
                break
            elif text == ':' and not assignment:
                target = True
            elif MAKE_ASSIGNMENT.fullmatch(text):
                assignment = True
            elif text == ';' and target and not assignment:
                start = token.end()
                yield from ((logical.start() + start + offset, comment)
                            for offset, comment in lex_comments('bash', line[start:]))
                break


def lex_comments(language, source):
    if language == 'text':
        yield from ((match.start(), match.group()) for match in re.finditer(r'^#[^\n]*', source, re.M))
        return
    if language == 'jq':
        yield from ((match.start(), match.group()) for match in JQ_TOKEN.finditer(source)
                    if match.group().startswith('#'))
        return
    if language == 'make':
        yield from make_comments(source)
        return
    if language == 'docker':
        directive_header = True
        for match in re.finditer(r'^[ \t]*(.*)$', source, re.M):
            text = match[1].rstrip()
            if directive_header and re.fullmatch(r'#\s*(?:syntax|escape|check)=\S.*', text, re.I):
                continue
            directive_header = False
            if text.startswith('#'):
                yield match.start(1), text
        for match in DOCKER_SHELL.finditer(source):
            if not match[1].lstrip().startswith('['):
                yield from ((match.start(1) + offset, text) for offset, text in lex_comments('bash', match[1]))
        return
    if not source.endswith('\n'):
        source += '\n'
    jq_filters = {match.start('command'): match for match in JQ_FILTER.finditer(source)} if language == 'bash' else {}
    for start, kind, value in lexer(language).get_tokens_unprocessed(source):
        if kind is Text and value == 'jq' and (match := jq_filters.get(start)):
            yield from ((match.start('program') + offset, text)
                        for offset, text in lex_comments('jq', match['program']))
        if language == 'bash' and kind is String and (match := HEREDOC.match(value)):
            body_start = value.find('\n') + 1
            body_end = value.rfind('\n') + 1
            body = value[body_start:body_end]
            if embedded := heredoc_language(match['label'], body):
                lex_body = re.sub(r'(?<!\\)\\(?=\$#)', ' ', body) if embedded == 'bash' and not match['quote'] else body
                yield from ((start + body_start + offset, body[offset:offset + len(text)])
                            for offset, text in lex_comments(embedded, lex_body))
        if kind not in Comment or kind in Comment.Preproc or kind in Comment.PreprocFile:
            continue
        text = value.strip('\r\n \t')
        start += len(value) - len(value.lstrip(' \t'))
        if start == 0 and text.startswith('#!') and language in {'bash', 'python'}:
            continue
        if language == 'go' and go_directive(source, start, text):
            continue
        if language == 'go' and (directive := GO_NOLINT.match(text)):
            tail = text[directive.end():]
            if not tail or tail.startswith((' ', '\t')):
                prose = tail.lstrip()
                if prose:
                    yield start + len(text) - len(prose), prose
                continue
        if language == 'objective-c' and text.startswith('//go:build ') and start == 0:
            continue
        if language == 'bash' and re.fullmatch(r'#\s*shellcheck\s+disable=SC\d+(?:,SC\d+)*', text):
            continue
        yield start, text


def mapping(node, key):
    if isinstance(node, MappingNode):
        return next((value for name, value in node.value if isinstance(name, ScalarNode) and name.value == key), None)
    return None


def workflow_scripts(source):
    root = yaml.compose(source, Loader=yaml.BaseLoader)
    jobs = mapping(root, 'jobs')
    if not isinstance(jobs, MappingNode):
        return
    workflow_shell = mapping(mapping(mapping(root, 'defaults'), 'run'), 'shell')
    for _, job in jobs.value:
        steps = mapping(job, 'steps')
        if not isinstance(steps, SequenceNode):
            continue
        for step in steps.value:
            run = mapping(step, 'run')
            if not isinstance(run, ScalarNode):
                continue
            shell = mapping(step, 'shell') or mapping(mapping(mapping(job, 'defaults'), 'run'), 'shell') or workflow_shell
            runner = mapping(job, 'runs-on')
            default = 'pwsh' if isinstance(runner, ScalarNode) and runner.value.startswith('windows-') else 'bash'
            language = {'bash': 'bash', 'sh': 'bash', 'python': 'python', 'pwsh': 'powershell'}.get(
                shell.value.split(maxsplit=1)[0] if isinstance(shell, ScalarNode) and shell.value.strip() else default
            )
            if not language:
                continue
            start = run.start_mark.index
            end = run.end_mark.index
            if run.style == '|':
                start = source.index('\n', start) + 1
                block = source[start:end]
                indent = min((len(line) - len(line.lstrip(' ')) for line in block.splitlines() if line.strip()), default=0)
                lines = block.splitlines(keepends=True)
                text = ''.join(line[indent:] if line.strip() else '\n' for line in lines)
                offsets = [0] + [match.end() for match in re.finditer('\n', text)]
                raw_offsets = [0] + [match.end() for match in re.finditer('\n', block)]
                for offset, comment in lex_comments(language, text):
                    line = bisect_right(offsets, offset) - 1
                    yield start + raw_offsets[line] + indent + offset - offsets[line], comment
            else:
                cursor = start
                for _, comment in lex_comments(language, run.value):
                    first_line = comment.split('\n', 1)[0]
                    position = source.find(first_line, cursor, end)
                    if position < 0:
                        position = source.find(first_line.split(maxsplit=1)[0], cursor, end)
                    if position < 0:
                        position = start
                    cursor = position + len(first_line)
                    yield position, comment


def comment_lines(path, source):
    language = source_language(path)
    if language is None:
        return
    tokens = set(lex_comments(language, source))
    if path.startswith('.github/workflows/') and language == 'yaml':
        tokens.update(workflow_scripts(source))
    offsets = [0] + [match.end() for match in re.finditer('\n', source)]
    for start, text in sorted(tokens):
        if path == 'go.mod' and text == '// indirect':
            continue
        number = bisect_right(offsets, start)
        line_start = offsets[number - 1]
        segments = text.split('\n')
        for index, segment in enumerate(segments):
            prefix = source[line_start:start] if not index else ''
            suffix = ''
            if index == len(segments) - 1:
                end = offsets[number + index] if number + index < len(offsets) else len(source)
                suffix = source[start + len(text):end]
            inline = bool(prefix.strip() or suffix.strip())
            column = len(prefix) + len(segment) - len(segment.lstrip()) + 1
            yield CommentLine(number + index, column, segment.strip(), inline)


def git(*args):
    return subprocess.check_output(['git', *args])


def paths(*args):
    return {os.fsdecode(path) for path in git(*args).split(b'\0') if path}


def source_text(path):
    file = Path(path)
    if file.is_symlink() or not file.is_file():
        return ''
    return file.read_text(encoding='utf-8')


def head_source(base, path):
    return git('show', f'{base}:{path}').decode('utf-8').replace('\r\n', '\n')


def revision(ref):
    return git('rev-parse', '--verify', '--end-of-options', f'{ref}^{{commit}}').decode().strip()


def additions(path, before, after):
    budget = Counter(line.text for line in comment_lines(path, before))
    for line in comment_lines(path, after):
        if budget[line.text] > 0:
            budget[line.text] -= 1
        else:
            yield line


def main():
    parser = argparse.ArgumentParser(description='Ban prose comments in first-party source.')
    parser.add_argument('--base', help='Git revision used as the legacy budget or inventory source')
    parser.add_argument('--inventory', action='store_true', help='Print remaining comments and removable line totals')
    args = parser.parse_args()
    os.chdir(os.fsdecode(git('rev-parse', '--show-toplevel').rstrip(b'\n')))
    untracked = paths('ls-files', '--others', '--exclude-standard', '-z')
    if args.inventory:
        base = revision(args.base) if args.base else None
        files = paths('ls-tree', '-r', '--name-only', '-z', base) if base else paths('ls-files', '--cached', '-z') | untracked
        own_lines = inline_lines = 0
        for path in sorted(files):
            if not source_language(path):
                continue
            source = head_source(base, path) if base else source_text(path)
            lines = list(comment_lines(path, source))
            for line in lines:
                print(f'{path}:{line.number}:{line.column}: {line.text}')
            by_number = {line.number for line in lines}
            inline = {line.number for line in lines if line.inline}
            own_lines += len(by_number - inline)
            inline_lines += len(inline)
        print(f'{own_lines} comment-only lines; {inline_lines} lines with inline comments', file=sys.stderr)
        return 0

    base = revision(args.base) if args.base else None
    if base:
        files = paths('diff', '--name-only', '--no-renames', '-z', base, '--') | untracked
        known = paths('ls-tree', '-r', '--name-only', '-z', base)
    else:
        files = paths('ls-files', '--cached', '-z') | untracked
        known = set()
    description = 'new comment' if base else 'comment'
    failures = 0
    for path in sorted(files):
        if not source_language(path) or not (after := source_text(path)):
            continue
        before = head_source(base, path) if path in known else ''
        for line in additions(path, before, after):
            print(f'{path}:{line.number}:{line.column}: {description}: {line.text}', file=sys.stderr)
            failures += 1
    if failures:
        print(f'{failures} {description} lines; use code or an ADR instead.', file=sys.stderr)
    return bool(failures)


if __name__ == '__main__':
    sys.exit(main())
