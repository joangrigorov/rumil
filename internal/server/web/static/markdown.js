/* Rumil's Markdown renderer for the session console.
 *
 * It covers the subset a review summary uses: headings, paragraphs, emphasis,
 * inline and fenced code, lists, blockquotes, links, tables and rules. The
 * source is escaped before any tag is added, so model output cannot inject
 * markup; links are limited to http, https and mailto. The markup is styled by
 * app.css for the console rather than for a document page.
 */
(function () {
  "use strict";

  function escapeHTML(value) {
    return String(value)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function inline(raw) {
    var text = escapeHTML(raw);
    // A code span is the one place where emphasis and links stay literal.
    var parts = text.split(/(`[^`\n]*`)/g);
    var out = "";
    for (var i = 0; i < parts.length; i += 1) {
      var part = parts[i];
      if (!part) {
        continue;
      }
      if (part.length > 1 && part.charAt(0) === "`" && part.charAt(part.length - 1) === "`") {
        out += "<code>" + part.slice(1, -1) + "</code>";
        continue;
      }
      out += emphasis(part);
    }
    return out;
  }

  function emphasis(text) {
    text = text.replace(/\[([^\]]+)\]\(((?:https?:\/\/|mailto:)[^\s)]+)\)/g, function (_, label, url) {
      return '<a href="' + url + '" target="_blank" rel="noopener noreferrer">' + label + "</a>";
    });
    text = text.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    text = text.replace(/__([^_]+)__/g, "<strong>$1</strong>");
    text = text.replace(/(^|[\s(])\*([^*\n]+)\*/g, "$1<em>$2</em>");
    text = text.replace(/(^|[\s(])_([^_\n]+)_/g, "$1<em>$2</em>");
    text = text.replace(/~~([^~]+)~~/g, "<del>$1</del>");
    return text;
  }

  function listItemText(text) {
    return text.replace(/^\[( |x|X)\]\s+/, function (_, mark) {
      return mark === " " ? "☐ " : "☑ ";
    });
  }

  function splitRow(line) {
    return line
      .trim()
      .replace(/^\|/, "")
      .replace(/\|$/, "")
      .split("|")
      .map(function (cell) {
        return cell.trim();
      });
  }

  function isTableStart(lines, index) {
    if (index + 1 >= lines.length || lines[index].indexOf("|") < 0) {
      return false;
    }
    return /^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(lines[index + 1]);
  }

  function table(lines, index) {
    var header = splitRow(lines[index]);
    var body = [];
    var i = index + 2;
    while (i < lines.length && lines[i].indexOf("|") >= 0 && !/^\s*$/.test(lines[i])) {
      body.push(splitRow(lines[i]));
      i += 1;
    }
    var html = '<div class="md-table-wrap"><table><thead><tr>';
    html += header
      .map(function (cell) {
        return "<th>" + inline(cell) + "</th>";
      })
      .join("");
    html += "</tr></thead><tbody>";
    html += body
      .map(function (row) {
        var cells = [];
        for (var column = 0; column < header.length; column += 1) {
          cells.push("<td>" + inline(row[column] || "") + "</td>");
        }
        return "<tr>" + cells.join("") + "</tr>";
      })
      .join("");
    html += "</tbody></table></div>";
    return { html: html, next: i };
  }

  function startsBlock(line, lines, index) {
    return (
      /^\s*(```|~~~)/.test(line) ||
      /^\s*#{1,6}\s+/.test(line) ||
      /^\s*(?:[-*+]|\d+[.)])\s+/.test(line) ||
      /^\s*>/.test(line) ||
      /^\s*(?:---+|\*\*\*+|___+)\s*$/.test(line) ||
      isTableStart(lines, index)
    );
  }

  function render(source) {
    var lines = String(source || "")
      .replace(/\r\n?/g, "\n")
      .split("\n");
    var html = [];
    var i = 0;

    while (i < lines.length) {
      var line = lines[i];

      if (/^\s*$/.test(line)) {
        i += 1;
        continue;
      }

      var fence = /^\s*(```|~~~)\s*([\w-]*)\s*$/.exec(line);
      if (fence) {
        var marker = /^\s*(```|~~~)/.exec(line)[1];
        var language = fence[2];
        var code = [];
        i += 1;
        while (i < lines.length && !new RegExp("^\\s*" + marker + "\\s*$").test(lines[i])) {
          code.push(lines[i]);
          i += 1;
        }
        if (i < lines.length) {
          i += 1;
        }
        html.push(
          '<pre class="md-code"><code' +
            (language ? ' class="language-' + escapeHTML(language) + '"' : "") +
            ">" +
            escapeHTML(code.join("\n")) +
            "</code></pre>"
        );
        continue;
      }

      var heading = /^\s*(#{1,6})\s+(.*?)\s*#*\s*$/.exec(line);
      if (heading) {
        var level = heading[1].length;
        html.push("<h" + level + ">" + inline(heading[2]) + "</h" + level + ">");
        i += 1;
        continue;
      }

      if (/^\s*(?:---+|\*\*\*+|___+)\s*$/.test(line)) {
        html.push("<hr>");
        i += 1;
        continue;
      }

      if (/^\s*>/.test(line)) {
        var quote = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) {
          quote.push(lines[i].replace(/^\s*>\s?/, ""));
          i += 1;
        }
        html.push("<blockquote>" + render(quote.join("\n")) + "</blockquote>");
        continue;
      }

      var unordered = /^\s*[-*+]\s+(.*)$/.exec(line);
      if (unordered) {
        var bullets = [];
        while (i < lines.length) {
          var item = /^\s*[-*+]\s+(.*)$/.exec(lines[i]);
          if (!item) {
            break;
          }
          bullets.push("<li>" + inline(listItemText(item[1])) + "</li>");
          i += 1;
        }
        html.push("<ul>" + bullets.join("") + "</ul>");
        continue;
      }

      var ordered = /^\s*\d+[.)]\s+(.*)$/.exec(line);
      if (ordered) {
        var numbers = [];
        while (i < lines.length) {
          var entry = /^\s*\d+[.)]\s+(.*)$/.exec(lines[i]);
          if (!entry) {
            break;
          }
          numbers.push("<li>" + inline(listItemText(entry[1])) + "</li>");
          i += 1;
        }
        html.push("<ol>" + numbers.join("") + "</ol>");
        continue;
      }

      if (isTableStart(lines, i)) {
        var parsed = table(lines, i);
        html.push(parsed.html);
        i = parsed.next;
        continue;
      }

      var paragraph = [];
      while (i < lines.length && !/^\s*$/.test(lines[i]) && !startsBlock(lines[i], lines, i)) {
        paragraph.push(lines[i].trim());
        i += 1;
      }
      if (paragraph.length) {
        html.push("<p>" + paragraph.map(inline).join("<br>") + "</p>");
        continue;
      }

      // A block marker this renderer does not consume still has to advance.
      html.push("<p>" + inline(line) + "</p>");
      i += 1;
    }

    return html.join("\n");
  }

  window.RumilMarkdown = { render: render };
})();
