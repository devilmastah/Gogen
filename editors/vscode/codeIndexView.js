"use strict";

// withCodeIndexLine copies config and puts the latest index snapshot on it.
// A config frame cached before the last progress event must not paint over
// that snapshot when settings opens.
function withCodeIndexLine(config, index) {
  const view = Object.assign({}, config);
  if (index && (index.line || index.phase)) {
    view.codeIndexPhase = index.phase || "";
    view.codeIndexLine = index.line || "";
    view.codeIndexSeq = index.seq || 0;
  }
  return view;
}

// acceptCodeIndexSeq reports whether seq is the next index line to show.
// A missing sequence is the first line only. A later frame with no
// sequence, or an older sequence, is a config snapshot that lost the race
// with progress.
function acceptCodeIndexSeq(prevSeq, seq) {
  const n = Number(seq) || 0;
  const prev = Number(prevSeq) || 0;
  if (!n) return prev === 0;
  return n > prev;
}

module.exports = { withCodeIndexLine, acceptCodeIndexSeq };
