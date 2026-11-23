// Copy buttons for .hb-code blocks.
//
// Adds a small ghost button to every .hb-code block that copies the
// text of its <code> (or <pre>) element. Labels come from the block's
// data-copy-label / data-copied-label attributes so pages can
// translate them; English defaults otherwise. Without the Clipboard
// API (old browsers, insecure origins) no button is added.
//
//   <div class="hb-code" data-copy-label="Copy" data-copied-label="Copied">
//     <pre><code>go install example.com/tool@latest</code></pre>
//   </div>
//   <script src="{{ asset "uikit/js/public/copy.js" }}" defer></script>
(function () {
  "use strict";
  if (!navigator.clipboard) return;

  document.querySelectorAll(".hb-code").forEach(function (block) {
    var source = block.querySelector("code") || block.querySelector("pre");
    if (!source) return;
    var label = block.getAttribute("data-copy-label") || "Copy";
    var done = block.getAttribute("data-copied-label") || "Copied";
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "hb-btn hb-btn--ghost hb-btn--sm hb-code__copy";
    btn.textContent = label;
    btn.addEventListener("click", function () {
      navigator.clipboard.writeText(source.textContent).then(function () {
        btn.textContent = done;
        setTimeout(function () { btn.textContent = label; }, 1600);
      });
    });
    block.appendChild(btn);
  });
})();
