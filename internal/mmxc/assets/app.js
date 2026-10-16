function i18n(key) {
  return (window.MMXC_T && window.MMXC_T[key] !== undefined) ? window.MMXC_T[key] : key;
}
function i18nf(key, subs) {
  var s = i18n(key);
  if (subs) Object.keys(subs).forEach(function(k) { s = s.split("{" + k + "}").join(subs[k]); });
  return s;
}
// i18nfmt translates key and substitutes its %s placeholders positionally from
// the remaining arguments. Used where the same string is shared with the Go
// side, which formats it with printf-style %s.
function i18nfmt(key) {
  var args = Array.prototype.slice.call(arguments, 1);
  var i = 0;
  return i18n(key).replace(/%s/g, function () { return i < args.length ? args[i++] : "%s"; });
}

(function () {
  var match = /[?&]scroll=(\d+)/.exec(window.location.search);
  if (match) {
    window.scrollTo(0, parseInt(match[1], 10) || 0);
  }
})();

(function () {
  if (window.location.hash !== "#review-new-transaction") return;
  var panel = document.getElementById("review-new-transaction");
  if (!panel) return;
  panel.open = true;
  var date = panel.querySelector('input[name="date"]');
  if (date) date.focus();
})();

// ---------------------------------------------------------------------------
// Dialogs. Every overlay in the app is a native <dialog> opened with
// showModal(), which buys top-layer stacking, focus containment, focus restore
// on close, Escape-to-close and background inertness without hand-written
// bookkeeping. openDialog/closeDialog only guard against the state errors
// showModal()/close() raise when the dialog is already in the requested state.
// ---------------------------------------------------------------------------

function openDialog(dialog) {
  if (!dialog || dialog.open || !dialog.showModal) return;
  dialog.showModal();
}

function closeDialog(dialog) {
  if (!dialog || !dialog.open) return;
  dialog.close();
}

// topDialog returns the dialog that is currently painted highest. The top layer
// paints in promotion order, so the last open dialog in DOM order is a good
// enough proxy for the app's shallow stacks (a report modal, then busy).
function topDialog() {
  var open = document.querySelectorAll("dialog[open]");
  return open.length ? open[open.length - 1] : null;
}

// dialogBackdropClicked reports whether a click landed on the ::backdrop rather
// than inside the panel. A backdrop click targets the <dialog> element itself,
// but so does a click on its own padding, so the pointer position is checked
// against the panel box too.
function dialogBackdropClicked(dialog, event) {
  if (event.target !== dialog) return false;
  var rect = dialog.getBoundingClientRect();
  if (!rect.width && !rect.height) return false;
  return event.clientX < rect.left || event.clientX > rect.right ||
    event.clientY < rect.top || event.clientY > rect.bottom;
}

// appConfirm replaces window.confirm. It resolves true when the user accepts.
// Unlike the native dialog the button labels are translated by the app rather
// than the browser, the affirmative button can name the action ("Void" instead
// of "OK"), and it cannot be silently suppressed by the browser's "prevent this
// page from creating additional dialogs" opt-out — which would otherwise make a
// destructive action look like it silently did nothing.
function appConfirm(message, options) {
  var opts = options || {};
  var dialog = document.querySelector("[data-app-dialog]");
  if (!dialog || !dialog.showModal) {
    // No dialog support: fall back so a confirmation is never skipped outright.
    return Promise.resolve(window.confirm(message));
  }
  var text = dialog.querySelector("[data-app-dialog-message]");
  var ok = dialog.querySelector("[data-app-dialog-ok]");
  var cancel = dialog.querySelector("[data-app-dialog-cancel]");
  if (text) text.textContent = message;
  if (ok) {
    // Remember the server-translated default before the first override, so a
    // per-action label ("Void", "Delete") never becomes the standing one.
    if (!ok.dataset.defaultLabel) ok.dataset.defaultLabel = ok.textContent;
    ok.textContent = opts.ok || ok.dataset.defaultLabel;
  }
  if (cancel) cancel.hidden = !!opts.alert;
  dialog.classList.toggle("is-alert", !!opts.alert);

  return new Promise(function (resolve) {
    function done() {
      dialog.removeEventListener("close", done);
      if (ok && ok.dataset.defaultLabel) ok.textContent = ok.dataset.defaultLabel;
      resolve(dialog.returnValue === "ok");
    }
    dialog.addEventListener("close", done);
    dialog.returnValue = "";
    openDialog(dialog);
  });
}

// appAlert replaces window.alert: same dialog with only the dismiss button.
function appAlert(message) {
  return appConfirm(message, { alert: true, ok: i18n("OK") });
}

document.addEventListener("click", function (event) {
  var dialog = event.target.closest ? event.target.closest("dialog") : null;
  if (dialog && dialog.hasAttribute("data-dismiss-on-backdrop") && dialogBackdropClicked(dialog, event)) {
    closeDialog(dialog);
  }
});

(function () {
  function wireReportModal(kind) {
    var modal = document.querySelector("[data-" + kind + "-modal]");
    if (!modal) return;
    var body = modal.querySelector("[data-" + kind + "-modal-body]");
    var close = modal.querySelector("[data-" + kind + "-close]");
    var openSelector = "[data-" + kind + "-open]";
    var openKey = kind + "Open";

    function show(id) {
      var detail = document.getElementById(id);
      if (!detail || !body) return;
      body.innerHTML = detail.innerHTML;
      openDialog(modal);
    }

    // The cloned detail markup is dropped on close so a stale report body can
    // never flash on the next open. Escape closes via the dialog's own "cancel"
    // event, which also lands here.
    modal.addEventListener("close", function () {
      if (body) body.innerHTML = "";
    });

    document.addEventListener("click", function (event) {
      var row = event.target.closest ? event.target.closest(openSelector) : null;
      if (row) {
        show(row.dataset[openKey]);
        return;
      }
      if (event.target.closest && event.target.closest("[data-" + kind + "-close]")) {
        closeDialog(modal);
      }
    });

    document.addEventListener("keydown", function (event) {
      if (event.key !== "Enter" && event.key !== " ") return;
      var row = event.target.closest ? event.target.closest(openSelector) : null;
      if (!row) return;
      event.preventDefault();
      show(row.dataset[openKey]);
    });

    if (close) close.addEventListener("click", function () { closeDialog(modal); });
  }

  wireReportModal("recurring");
  wireReportModal("anomaly");
  wireReportModal("budget");
  wireReportModal("category");
  wireReportModal("payee");
  wireReportModal("trend");
})();

// Version label in the header opens a modal with the embedded commit history.
// Its content is rendered server-side, so unlike wireReportModal it has nothing
// to clone or clear.
(function () {
  var modal = document.querySelector("[data-version-modal]");
  if (!modal) return;

  document.addEventListener("click", function (event) {
    if (!event.target.closest) return;
    if (event.target.closest("[data-version-open]")) { openDialog(modal); return; }
    if (event.target.closest("[data-version-close]")) closeDialog(modal);
  });

  document.addEventListener("keydown", function (event) {
    if (event.key !== "Enter" && event.key !== " ") return;
    if (event.target.closest && event.target.closest("[data-version-open]")) {
      event.preventDefault();
      openDialog(modal);
    }
  });
})();

// Review's Learned Mappings modal. Saving or deleting a mapping is a plain form
// post that redirects back with rules=1, so the dialog reopens for the next fix.
(function () {
  var modal = document.querySelector("[data-rules-modal]");
  if (!modal) return;

  document.addEventListener("click", function (event) {
    if (!event.target.closest) return;
    if (event.target.closest("[data-rules-open]")) { openDialog(modal); return; }
    if (event.target.closest("[data-rules-close]")) closeDialog(modal);
  });

  var url = new URL(window.location.href);
  if (url.searchParams.get("rules") === "1") {
    openDialog(modal);
    url.searchParams.delete("rules");
    history.replaceState(null, "", url.pathname + url.search + url.hash);
  }
})();

(function () {
  // Every settings section that picks a model carries [data-llm-settings] with
  // its own "Query Models" button, but the endpoint is configured once, in the
  // LLM section. So the URL and the target datalist are resolved document-wide:
  // querying from Translation must reach the URL input it does not own, and one
  // query fills the single shared <datalist> every model input points at.
  var sections = document.querySelectorAll("[data-llm-settings]");
  if (!sections.length) return;
  var urlInput = document.querySelector("[data-llm-url]");
  var options = document.getElementById("llm-model-options");

  function replaceOptions(models) {
    if (!options) return;
    options.innerHTML = "";
    models.forEach(function (model) {
      var option = document.createElement("option");
      option.value = model;
      options.appendChild(option);
    });
  }

  function queryModels(button, status) {
    function setStatus(text) {
      if (status) status.textContent = text;
    }
    var endpoint = urlInput ? urlInput.value.trim() : "";
    if (!endpoint) {
      setStatus(i18n("Enter a base URL before querying models."));
      return;
    }
    button.disabled = true;
    setStatus(i18n("Querying models..."));
    fetch("/settings/llm-models?url=" + encodeURIComponent(endpoint), { headers: { "Accept": "application/json" } })
      .then(function (response) {
        return response.json().then(function (body) {
          if (!response.ok) throw new Error(body.error || i18n("Model query failed"));
          return body;
        });
      })
      .then(function (body) {
        var models = body.models || [];
        if (!models.length) {
          setStatus(i18n("No models were returned by this endpoint."));
          return;
        }
        replaceOptions(models);
        setStatus(models.length === 1 ? i18n("Loaded 1 model.") : i18nf("Loaded {n} models.", { n: models.length }));
      })
      .catch(function (error) {
        setStatus(error.message || i18n("Model query failed"));
      })
      .finally(function () {
        button.disabled = false;
      });
  }

  sections.forEach(function (section) {
    var button = section.querySelector("[data-llm-models]");
    if (!button) return;
    var status = section.querySelector("[data-llm-status]");
    button.addEventListener("click", function () { queryModels(button, status); });
  });
})();

document.addEventListener("input", function (event) {
  var input = event.target;
  if (!(input instanceof HTMLInputElement)) return;
  var hiddenName = input.dataset.combobox;
  if (!hiddenName) return;

  var form = input.form;
  if (!form) return;
  var scope = input.closest(".split-line") || form;
  var hidden = scope.querySelector('input[name="' + hiddenName + '"]');
  if (!hidden) return;

  hidden.value = "";
  var option = datalistOption(input.getAttribute("list"), input.value);
  if (option) hidden.value = option.dataset.id || "";
});

// Picking a payee fills an empty Category with that payee's default category
// (PAYEE_V1.CATEGID), the same pre-fill MMEX does. Only a blank field is ever
// touched: a category the user, the edited transaction, or the statement
// importer already chose is left alone. Review candidates are the exception:
// there the payee is a per-row correction of the importer's guess, so manually
// changing it replaces the category with the new payee's default (the importer's
// own values never fire an input event). Registered before the candidate-row
// validator below so review rows re-validate against the filled-in category.
document.addEventListener("input", function (event) {
  var input = event.target;
  if (!(input instanceof HTMLInputElement)) return;
  // data-category-id is emitted only on payee <option>s, so finding one is what
  // identifies this input as a payee combobox on a resolved payee.
  var payeeOption = datalistOption(input.getAttribute("list"), input.value);
  var categoryID = payeeOption ? payeeOption.dataset.categoryId : "";
  if (!categoryID) return;

  // Review candidates share one form across every row, so the row is the scope
  // there; the single-transaction forms are their own.
  var candidateRow = input.closest("[data-candidate-row]");
  var scope = candidateRow || input.form;
  if (!scope) return;
  // "category_text" in the transaction forms, "category" in candidate rows;
  // split lines use split_category_text and are deliberately not matched.
  var category = scope.querySelector('input[name="category_text"], input[name="category"]');
  if (!category || (category.value.trim() && !candidateRow)) return;
  // Split mode hides the single category (disabled in forms, a hidden wrapper
  // in candidate rows) — leave it alone, the split lines carry the categories.
  if (category.disabled || category.closest("[hidden]")) return;

  // A default category the form doesn't offer (top-level, or hidden by the
  // inactive-items setting) has no option to copy, so leave the field empty
  // rather than filling in something the form would reject.
  var categoryOption = datalistOptionByID(category.getAttribute("list"), categoryID);
  if (!categoryOption) return;
  category.value = categoryOption.value;
  if (category.dataset.combobox) {
    var categoryHidden = scope.querySelector('input[name="' + category.dataset.combobox + '"]');
    if (categoryHidden) categoryHidden.value = categoryID;
  }
});

function closestField(control) {
  return control ? control.closest("label, .split-editor") : null;
}

function setFieldVisible(field, visible) {
  if (!field) return;
  field.hidden = !visible;
  field.querySelectorAll("input, select, button").forEach(function (control) {
    if (control.type === "hidden") return;
    control.disabled = !visible;
  });
}

function splitSuffix(editor) {
  return (editor && editor.dataset.splitSuffix) || "";
}

// One term of an amount expression: digits with an optional decimal point,
// matching what parseAmountExpression accepts on the Go side.
var AMOUNT_NUMBER = /^(\d+\.?\d*|\.\d+)$/;

// evalAmount reads an amount field: a plain decimal number, or a chain of
// additions and subtractions such as "123.25 + 20-0.01". Returns NaN when the
// text is not a valid amount expression. Mirrors parseAmountExpression in
// helpers.go — the server re-evaluates every amount it receives, so this is
// what lets running totals and validation keep up while the user types.
function evalAmount(text) {
  var expr = String(text === undefined || text === null ? "" : text).replace(/\s/g, "");
  if (!expr) return NaN;
  var sign = 1;
  var index = 0;
  // A leading sign belongs to the first term rather than being an operator.
  while (index < expr.length && (expr[index] === "+" || expr[index] === "-")) {
    if (expr[index] === "-") sign = -sign;
    index++;
  }
  var total = 0;
  var terms = 0;
  while (index < expr.length) {
    if (terms > 0) {
      if (expr[index] !== "+" && expr[index] !== "-") return NaN;
      sign = expr[index] === "-" ? -1 : 1;
      index++;
    }
    var start = index;
    while (index < expr.length && expr[index] !== "+" && expr[index] !== "-") index++;
    var token = expr.slice(start, index);
    if (!AMOUNT_NUMBER.test(token)) return NaN;
    total += sign * parseFloat(token);
    terms++;
  }
  if (terms === 0) return NaN;
  // Binary rounding noise from summing decimals sits below any currency's
  // smallest unit; drop it so the result reads as the amount the user meant.
  return terms > 1 ? Math.round(total * 1e4) / 1e4 : total;
}

// amountOf evaluates one amount input, treating anything unreadable as 0 so the
// callers that only add up numbers stay simple.
function amountOf(input) {
  var value = evalAmount(input ? input.value : "");
  return isFinite(value) ? value : 0;
}

// markAmountValidity flags an amount field that reads as neither a number nor a
// calculation. It is only a cue — the value is left alone and the server has the
// final say. Called on change rather than on input so a half-typed "12+" isn't
// flagged mid-keystroke.
function markAmountValidity(input) {
  if (!input || !input.classList) return;
  var raw = input.value.trim();
  input.classList.toggle("amount-invalid", raw !== "" && !isFinite(evalAmount(raw)));
}

// normalizeAmountInput replaces a typed calculation with its result once the
// field is left, so what is submitted is what the user sees. Untouched when the
// text is not a valid expression: the field keeps the input for correction and
// the server reports the error.
function normalizeAmountInput(input) {
  if (!input || input.readOnly || input.disabled) return;
  var raw = input.value.trim();
  if (raw === "") return;
  var value = evalAmount(raw);
  if (!isFinite(value)) return;
  var normalized = value.toFixed(2);
  if (normalized !== raw) input.value = normalized;
}

// sumSplits totals the enabled split-amount inputs in one editor, honoring its
// data-split-suffix so the same code serves single forms ("") and the review
// candidate rows ("_<i>") that share one form and need unique field names.
function sumSplits(editor) {
  if (!editor) return 0;
  var total = 0;
  editor.querySelectorAll('input[name="split_amount' + splitSuffix(editor) + '"]').forEach(function (input) {
    if (!input.disabled) total += amountOf(input);
  });
  return total;
}

// refreshSplitEditor recomputes one editor's total and propagates it to whatever
// owns the editor: an import-candidate row (sync its read-only amount and
// re-validate) or a standalone transaction form (sync amount_display and the
// type/split visibility via updateTransactionForm).
function refreshSplitEditor(editor) {
  if (!editor) return;
  var row = editor.closest("[data-candidate-row]");
  if (row) {
    // updateCandidateRow owns the candidate's amount sync and validation.
    updateCandidateRow(row);
    return;
  }
  var form = editor.closest("[data-transaction-form]");
  if (form) updateTransactionForm(form);
}

// setSplitTotalDisplay updates a single form's running "Split total" label and
// mirrors it into the disabled amount_display shown when editing a split parent.
function setSplitTotalDisplay(form) {
  var editor = form.querySelector("[data-split-editor]");
  var total = editor ? sumSplits(editor) : 0;
  var target = form.querySelector("[data-split-total]");
  if (target) target.textContent = i18n("Split total") + " " + moneyFmt(total);
  var display = form.querySelector('input[name="amount_display"]');
  if (display) display.value = total.toFixed(2);
}

function updateTransactionForm(form) {
  var typeControl = form.querySelector('select[name="type"], input[name="type"]');
  var type = typeControl ? typeControl.value : "";
  var editor = form.querySelector("[data-split-editor]");
  var splitBox = form.querySelector('input[type="checkbox"][name="split_mode"]');
  var splitHidden = form.querySelector('input[type="hidden"][name="split_mode"][value="true"]');
  var canSplit = type !== "Transfer";

  if (splitBox) {
    splitBox.disabled = !canSplit;
    if (!canSplit) splitBox.checked = false;
  }
  var split = !!(((splitBox && splitBox.checked) || splitHidden) && canSplit);

  setFieldVisible(closestField(form.querySelector('[name="to_account_id"]')), type === "Transfer");
  setFieldVisible(closestField(form.querySelector('[name="payee_text"]')), canSplit);
  setFieldVisible(closestField(form.querySelector('[name="category_text"]')), canSplit && !split);
  setFieldVisible(closestField(form.querySelector('[name="amount"]')), !split);

  // The split editor stays mounted whenever splitting is possible so its inline
  // "Split" checkbox (in the tools bar) is always reachable; only the split
  // lines + Add/total reveal once the box is checked. The split-active class
  // restores the editor's box framing (it's flattened to a bare row when off).
  if (editor) {
    editor.hidden = !canSplit;
    editor.classList.toggle("split-active", split);
    setFieldVisible(editor.querySelector("[data-split-lines]"), split);
    var addBtn = editor.querySelector("[data-split-add]");
    if (addBtn) addBtn.hidden = !split;
    var totalEl = editor.querySelector("[data-split-total]");
    if (totalEl) totalEl.hidden = !split;
  }
  setSplitTotalDisplay(form);
}

// cloneSplitLine duplicates the first split line of an editor, clears it, and
// refreshes totals. Serves both single forms and review candidate editors.
function cloneSplitLine(editor) {
  var lines = editor ? editor.querySelector("[data-split-lines]") : null;
  if (!lines) return;
  var source = lines.querySelector(".split-line");
  if (!source) return;
  var clone = source.cloneNode(true);
  clone.querySelectorAll("input").forEach(function (input) {
    input.value = "";
  });
  lines.appendChild(clone);
  refreshSplitEditor(editor);
}

document.querySelectorAll("[data-transaction-form]").forEach(updateTransactionForm);

(function () {
  var notes = document.querySelector('.edit-row [data-transaction-form] input[name="notes"]');
  if (!notes) return;
  notes.focus();
  if (notes.setSelectionRange) {
    var end = notes.value.length;
    notes.setSelectionRange(end, end);
  }
})();

function updateCandidateRow(row, fromCheckbox) {
  if (!row) return;
  var type = row.querySelector("[data-candidate-type]");
  var isTransfer = !!type && (type.value === "TransferOut" || type.value === "TransferIn");
  row.classList.toggle("candidate-transfer", isTransfer);

  // A per-row Split toggle reuses the shared split editor: when active the
  // category combobox is hidden and the read-only amount tracks the split total;
  // when inactive the editor's inputs are disabled so they aren't submitted and
  // the single category applies. (The server infers a split purely from the
  // presence of split_amount_<i> fields, so disabling is what turns it off.)
  var splitToggle = row.querySelector("[data-candidate-split-toggle]");
  var splitEditor = row.querySelector("[data-split-editor]");
  var categoryField = row.querySelector("[data-candidate-category]");
  var amount = row.querySelector('input[name="amount"]');
  if (splitToggle) splitToggle.disabled = isTransfer;
  var splitActive = !isTransfer && !!(splitToggle && splitToggle.checked);
  if (splitEditor) {
    splitEditor.hidden = !splitActive;
    splitEditor.querySelectorAll("input").forEach(function (input) {
      input.disabled = !splitActive;
    });
  }
  if (categoryField) categoryField.hidden = splitActive;
  if (amount) {
    amount.readOnly = splitActive;
    if (splitActive) amount.value = sumSplits(splitEditor).toFixed(2);
  }

  // The statement-amount label is a fixed reference: flag it when the live
  // (for splits, calculated) amount no longer matches what's on the statement.
  var stmtLabel = row.querySelector("[data-statement-amount]");
  if (stmtLabel && amount) {
    var stmtAmt = parseFloat(stmtLabel.dataset.statementAmount || "0") || 0;
    var liveAmt = amountOf(amount);
    stmtLabel.classList.toggle("mismatch", Math.abs(liveAmt - stmtAmt) >= 0.005);
  }

  // Each message is routed to the cell it concerns: "payee" under the payee /
  // to-account cell, "category" under the category cell, "general" under the
  // date cell. errors/warnings keep the flat counts for row styling and the
  // import checkbox. Mirrors the server-side grouping in finalizeImportCandidate.
  var errors = [];
  var warnings = [];
  var general = [];
  var payeeMsgs = [];
  var categoryMsgs = [];
  function push(bucket, text, isError) {
    bucket.push({ text: text, isError: isError });
    (isError ? errors : warnings).push(text);
  }
  var date = row.querySelector('input[name="date"]');
  var payee = row.querySelector('input[name="payee"]');
  var category = row.querySelector('input[name="category"]');
  var toAccount = row.querySelector('select[name="to_account_id"]');
  var status = row.querySelector('select[name="status"]');
  var duplicate = row.dataset.duplicate === "true";

  if (!candidateDateValid(date)) push(general, "date must be YYYY-MM-DD", true);
  if (!type || ["Withdrawal", "Deposit", "TransferOut", "TransferIn"].indexOf(type.value) === -1) push(general, "type must be Withdrawal, Deposit, or Transfer", true);
  if (!amount || !(amountOf(amount) > 0)) push(general, "amount must be greater than zero", true);
  if (status && ["", "__blank", "Reconciled", "Follow up"].indexOf(status.value) === -1) push(general, i18n("transaction status is not MMEX-compatible"), true);

  if (isTransfer) {
    if (!toAccount || !toAccount.value) push(payeeMsgs, i18n(isTransferIn ? "from account is required for transfers" : "to account is required for transfers"), true);
  } else {
    if (!payee || !payee.value.trim()) {
      push(payeeMsgs, i18n("payee is required"), true);
    } else if (!datalistHasValue("review-payee-options", payee.value)) {
      push(payeeMsgs, i18n("Unknown payee; importing will create it"), false);
    }
    if (splitActive) {
      var splitErrs = [];
      validateCandidateSplits(splitEditor, amount, splitErrs);
      splitErrs.forEach(function (m) { push(categoryMsgs, m, true); });
    } else if (!category || !category.value.trim()) {
      push(categoryMsgs, i18n("category is required"), true);
    } else if (!datalistHasValue("review-category-options", category.value)) {
      if (isNewCategoryText(category.value)) {
        push(categoryMsgs, i18n("Unknown category; importing will create it"), false);
      } else {
        push(categoryMsgs, i18nf("category \"{cat}\" was not found", { cat: category.value }), true);
      }
    }
  }
  if (duplicate) push(general, i18n("Possible duplicate"), false);

  function fillMessages(field, items) {
    var box = row.querySelector('[data-messages="' + field + '"]');
    if (!box) return;
    box.innerHTML = "";
    items.forEach(function (m) {
      var el = document.createElement("small");
      el.className = m.isError ? "error-text" : "warning-text";
      el.textContent = m.text;
      box.appendChild(el);
    });
  }
  fillMessages("general", general);
  fillMessages("payee", payeeMsgs);
  fillMessages("category", categoryMsgs);

  row.classList.toggle("candidate-error", errors.length > 0);
  row.classList.toggle("candidate-duplicate", errors.length === 0 && duplicate);
  row.classList.toggle("candidate-warning", errors.length === 0 && !duplicate && warnings.length > 0);

  var checkbox = row.querySelector('input[name="approved"]');
  if (checkbox) {
    if (fromCheckbox === true && errors.length === 0) {
      checkbox.dataset.userToggled = "true";
    }
    if (errors.length > 0) {
      // An invalid row can never be imported, even if clicked directly.
      checkbox.checked = false;
    } else if (fromCheckbox !== true && checkbox.dataset.userToggled !== "true") {
      // Auto-manage selection on edits, but respect a manual click: valid rows
      // default to selected, duplicates default to skipped (opt-in upsert).
      checkbox.checked = !duplicate;
    }
  }
  updateDuplicateVisibility();
  updateClosingBanner();
  updateImportButton();
}

// validateCandidateSplits pushes any split-line problems for an import candidate
// onto errors: each line needs a known category and a positive amount, there
// must be at least two lines, and they must sum to the row amount.
function validateCandidateSplits(editor, amountInput, errors) {
  var suffix = splitSuffix(editor);
  var lines = editor.querySelectorAll(".split-line");
  if (lines.length < 2) errors.push(i18n("split transactions require at least two split lines"));
  var total = 0;
  lines.forEach(function (line, n) {
    var cat = line.querySelector('input[name="split_category_text' + suffix + '"]');
    var amt = line.querySelector('input[name="split_amount' + suffix + '"]');
    var amtVal = amountOf(amt);
    if (!cat || !cat.value.trim()) {
      errors.push(i18nf("split line {n} category is required", { n: n + 1 }));
    } else if (!datalistHasValue("review-category-options", cat.value)) {
      errors.push(i18nf("split line {n} category \"{cat}\" was not found", { n: n + 1, cat: cat.value }));
    }
    if (!(amtVal > 0)) errors.push(i18nf("split line {n} amount must be greater than zero", { n: n + 1 }));
    total += amtVal;
  });
  var amountVal = amountOf(amountInput);
  if (Math.abs(total - amountVal) >= 0.005) errors.push(i18n("split lines must sum to the amount"));
}

function moneyFmt(v) {
  var s = Math.abs(v).toFixed(2);
  var dot = s.indexOf(".");
  var intPart = dot >= 0 ? s.slice(0, dot) : s;
  var fracPart = dot >= 0 ? s.slice(dot) : "";
  intPart = intPart.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return (v < 0 ? "-" : "") + "$" + intPart + fracPart;
}

// candidateDeltaSum totals the signed amounts of the candidates that will
// actually be inserted: selected, valid, non-duplicate rows dated on or before
// the closing date. Duplicates are excluded because the account's balance
// already reflects the existing row.
function candidateDeltaSum(closingDate) {
  var sum = 0;
  document.querySelectorAll("[data-candidate-row]").forEach(function (row) {
    if (row.classList.contains("candidate-error")) return;
    if (row.dataset.duplicate === "true") return;
    var checkbox = row.querySelector('input[name="approved"]');
    if (!checkbox || !checkbox.checked) return;
    var dateInput = row.querySelector('input[name="date"]');
    var date = dateInput ? dateInput.value.trim() : "";
    if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) return;
    if (closingDate && date > closingDate) return;
    var amountInput = row.querySelector('input[name="amount"]');
    var amount = amountOf(amountInput);
    if (!(amount > 0)) return;
    var typeSel = row.querySelector("[data-candidate-type]");
    var type = typeSel ? typeSel.value : "Withdrawal";
    sum += type === "Deposit" || type === "TransferIn" ? amount : -amount;
  });
  return sum;
}

// updateImportButton keeps the count of ticked candidates on the import button
// current, so an empty selection is visible before the button is ever pressed
// rather than only being reported after a submit. The count is digits and
// parentheses, so it needs no translation of its own.
function updateImportButton() {
  document.querySelectorAll("[data-import-count]").forEach(function (slot) {
    var form = slot.closest("form");
    if (!form) return;
    var selected = form.querySelectorAll('input[type="checkbox"][name="approved"]:checked').length;
    slot.textContent = "(" + selected + ")";
  });
}

// updateClosingBanner recomputes the closing-balance projection from the base
// account balance plus the currently selected candidates, then rewrites the
// banner text and match/mismatch styling.
function updateClosingBanner() {
  var banner = document.querySelector("[data-closing-banner]");
  if (!banner) return;
  var base = parseFloat(banner.dataset.closingBase || "0");
  var expected = parseFloat(banner.dataset.closingExpected || "0");
  var closingDate = banner.dataset.closingDate || "";
  var closingDateDisplay = banner.dataset.closingDateDisplay || closingDate;
  var projected = base + candidateDeltaSum(closingDate);
  var delta = projected - expected;
  var matches = Math.abs(delta) < 0.005;
  if (banner.dataset.tentative !== "true") {
    banner.classList.toggle("match", matches);
    banner.classList.toggle("mismatch", !matches);
  }
  var textEl = banner.querySelector("[data-closing-text]");
  if (!textEl) return;
  if (matches) {
    textEl.innerHTML =
      "<strong>" + i18n("✓ Closing balance checks out.") + "</strong> " +
      i18nfmt("After importing the selected candidates, the account balance on %s will be %s, matching the statement closing balance.",
        closingDateDisplay, moneyFmt(projected));
  } else {
    textEl.innerHTML =
      "<strong>" + i18n("✗ Closing balance mismatch.") + "</strong> " +
      i18nfmt("The statement closing balance on %s expects %s, but importing the selected candidates projects %s (off by %s).",
        closingDateDisplay, moneyFmt(expected), moneyFmt(projected), moneyFmt(delta));
  }
}

function candidateDateValid(input) {
  if (!input || !input.value) return false;
  return /^\d{4}-\d{2}-\d{2}$/.test(input.value || "");
}

// isNewCategoryText reports whether text reads "Parent: Sub" for an existing
// top-level Parent (the review-category-parents list, which — unlike the
// selectable categories — always carries top-levels) and a Sub that is not
// already offered. The server re-checks and creates it on import; this only
// decides between a "will create" notice and a "not found" error.
function isNewCategoryText(text) {
  var at = text.indexOf(":");
  if (at < 0) return false;
  var parent = text.slice(0, at).trim().toLowerCase();
  var sub = text.slice(at + 1).trim();
  if (!parent || !sub || sub.indexOf(":") >= 0) return false;
  var wanted = parent + ": " + sub.toLowerCase();
  var options = document.getElementById("review-category-options");
  if (options) {
    for (var i = 0; i < options.options.length; i++) {
      if (options.options[i].value.toLowerCase() === wanted) return false;
    }
  }
  var parents = document.getElementById("review-category-parents");
  if (!parents) return false;
  for (var j = 0; j < parents.options.length; j++) {
    if (parents.options[j].value.toLowerCase() === parent) return true;
  }
  return false;
}

function datalistHasValue(id, value) {
  return !!datalistOption(id, value);
}

// datalistOption / datalistOptionByID look one <option> up in a datalist, by
// its displayed value or by the row id carried in data-id. Both return null for
// an unknown list so callers can treat "no list" and "no match" alike.
function datalistOption(id, value) {
  var list = id ? document.getElementById(id) : null;
  if (!list) return null;
  for (var i = 0; i < list.options.length; i++) {
    if (list.options[i].value === value) return list.options[i];
  }
  return null;
}

function datalistOptionByID(listID, id) {
  var list = listID ? document.getElementById(listID) : null;
  if (!list) return null;
  for (var i = 0; i < list.options.length; i++) {
    if (list.options[i].dataset.id === id) return list.options[i];
  }
  return null;
}

// The overlay lookup is scoped to dialog[…] on purpose: <body> also carries a
// data-global-busy flag (CSS uses it for the wait cursor) and, being first in
// tree order, an unscoped [data-global-busy] selector matches the body instead.
function busyOverlay() {
  return document.querySelector("dialog[data-global-busy]");
}

function showGlobalBusy(label) {
  var overlay = busyOverlay();
  if (!overlay) return;
  var labelEl = overlay.querySelector("[data-global-busy-label]");
  if (labelEl) {
    if (!overlay.dataset.defaultLabel) overlay.dataset.defaultLabel = labelEl.textContent;
    labelEl.textContent = label || overlay.dataset.defaultLabel || labelEl.textContent;
  }
  // Opened last so it is promoted to the top of the top layer, above any report
  // modal that is still open behind it (e.g. voiding a row from inside one).
  openDialog(overlay);
  document.body.dataset.globalBusy = "true";
  document.body.setAttribute("aria-busy", "true");
}

function hideGlobalBusy() {
  closeDialog(busyOverlay());
  delete document.body.dataset.globalBusy;
  document.body.removeAttribute("aria-busy");
}

// localSubmissionError returns the message for a rejection the page can decide
// on its own, so the most common one costs no round trip at all.
// data-require-checked names a checkbox group that needs at least one tick.
function localSubmissionError(form) {
  var group = form.dataset.requireChecked;
  if (!group) return "";
  if (form.querySelector('input[type="checkbox"][name="' + group + '"]:checked')) return "";
  return form.dataset.requireCheckedMessage || "";
}

// preflightSubmission resolves true when the form may be submitted for real. It
// posts the form to a read-only validation endpoint that runs the same checks
// the real handler runs, and reports a rejection in a dialog over the intact
// page. A transport failure resolves true on purpose: a flaky pre-flight must
// never be the reason an import cannot be attempted — the real handler still
// rejects anything invalid.
function preflightSubmission(form) {
  var local = localSubmissionError(form);
  if (local) {
    return appAlert(local).then(function () {
      var first = form.querySelector('input[type="checkbox"][name="' + form.dataset.requireChecked + '"]');
      if (first) {
        first.scrollIntoView({ block: "nearest" });
        first.focus({ preventScroll: true });
      }
      return false;
    });
  }
  if (!form.dataset.preflight) return Promise.resolve(true);
  if (!window.fetch || !window.FormData || !window.URLSearchParams) return Promise.resolve(true);
  var body = new URLSearchParams();
  new FormData(form).forEach(function (value, name) {
    body.append(name, value);
  });
  return fetch(form.dataset.preflight, {
    method: "POST",
    body: body,
    headers: { "X-MMXC-Partial": "true" }
  }).then(function (response) {
    if (response.ok) return true;
    return response.text().then(function (text) {
      return appAlert(text.trim() || i18n("Could not validate this import. Please try again.")).then(function () {
        return false;
      });
    });
  }).catch(function () {
    return true;
  });
}

function submitWithBusy(form, label) {
  if (!form) return;
  showGlobalBusy(label);
  form.submit();
}

function navigateWithBusy(url, replace, label) {
  showGlobalBusy(label);
  if (replace) {
    window.location.replace(url);
  } else {
    window.location = url;
  }
}

(function () {
  window.addEventListener("pageshow", hideGlobalBusy);

  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (!form || form.dataset.noGlobalBusy === "true") return;
    if ((form.target || "").toLowerCase() === "_blank") return;
    // A method="dialog" form closes its dialog instead of navigating, so the
    // busy overlay would never be cleared by the pageshow that normally hides
    // it — it would sit there with a progress cursor over a live page.
    if ((form.getAttribute("method") || "").toLowerCase() === "dialog") return;
    // window.confirm was synchronous, so a false return could cancel the submit
    // in place. The dialog resolves later, so the submit is always cancelled and
    // then replayed on accept. requestSubmit re-fires this handler (and carries
    // the original submitter, which forms like candidate dismissal depend on for
    // their formaction), so a one-shot flag keeps it from asking twice.
    if (form.dataset.confirm && form.dataset.confirmed !== "true") {
      event.preventDefault();
      var submitter = event.submitter;
      appConfirm(form.dataset.confirm, { ok: form.dataset.confirmOk }).then(function (accepted) {
        if (!accepted) return;
        form.dataset.confirmed = "true";
        if (form.requestSubmit) {
          form.requestSubmit(submitter);
        } else {
          showGlobalBusy(form.dataset.busyLabel);
          form.submit();
        }
        delete form.dataset.confirmed;
      });
      return;
    }
    // Same replay shape as the confirm above, for forms whose server-side
    // rejection would otherwise land the user on a bare error page and discard
    // everything they had typed. The submission is checked first and only
    // replayed once it is known to be acceptable.
    if ((form.dataset.preflight || form.dataset.requireChecked) && form.dataset.preflighted !== "true") {
      event.preventDefault();
      var preflightSubmitter = event.submitter;
      preflightSubmission(form).then(function (accepted) {
        if (!accepted) return;
        form.dataset.preflighted = "true";
        if (form.requestSubmit) {
          form.requestSubmit(preflightSubmitter);
        } else {
          showGlobalBusy(form.dataset.busyLabel);
          form.submit();
        }
        delete form.dataset.preflighted;
      });
      return;
    }
    showGlobalBusy(form.dataset.busyLabel);
  }, true);

  document.addEventListener("click", function (event) {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    if (!event.target.closest) return;
    var link = event.target.closest("a[href]");
    if (!link || link.target || link.hasAttribute("download")) return;
    var href = link.getAttribute("href");
    if (!href || href.charAt(0) === "#") return;
    var url;
    try {
      url = new URL(href, window.location.href);
    } catch (err) {
      return;
    }
    if (url.origin !== window.location.origin) return;
    if (url.pathname === window.location.pathname && url.search === window.location.search && url.hash) return;
    showGlobalBusy(link.dataset.busyLabel);
  }, true);
})();

(function () {
  var select = document.querySelector("[data-account-nav-filter]");
  if (!select) return;

  var key = "mmxc.accounts.navigationFilter.v1";
  var allowed = { favorites: true, open: true, closed: true, all: true };
  var items = Array.prototype.slice.call(document.querySelectorAll("[data-account-nav-item]"));
  var groups = Array.prototype.slice.call(document.querySelectorAll("[data-account-nav-group]"));

  function readFilter() {
    try {
      var value = window.localStorage.getItem(key);
      if (allowed[value]) return value;
    } catch (err) {
      // The default is fine when localStorage is unavailable.
    }
    return "open";
  }

  function writeFilter(value) {
    try {
      window.localStorage.setItem(key, value);
    } catch (err) {
      // Navigation filtering remains usable for the current page view.
    }
  }

  function itemVisible(item, filter) {
    var status = (item.dataset.accountStatus || "").toLowerCase();
    if (filter === "all") return true;
    if (filter === "favorites") return item.dataset.accountFavorite === "true";
    if (filter === "closed") return status === "closed";
    return status === "open";
  }

  function applyFilter(filter) {
    items.forEach(function (item) {
      item.hidden = !itemVisible(item, filter);
    });
    groups.forEach(function (group) {
      var typ = group.getAttribute("data-account-nav-group");
      var hasVisible = items.some(function (item) {
        return !item.hidden && item.getAttribute("data-account-type") === typ;
      });
      group.hidden = !hasVisible;
    });
  }

  select.value = readFilter();
  if (!allowed[select.value]) select.value = "open";
  applyFilter(select.value);
  select.addEventListener("change", function () {
    var value = allowed[select.value] ? select.value : "open";
    writeFilter(value);
    applyFilter(value);
  });
})();

(function () {
  var list = document.querySelector("[data-assistant-history]");
  if (!list) return;

  var key = "mmxc.assistant.questions.v1";
  var maxItems = 30;
  var promptInput = document.querySelector('[data-assistant-prompt-form] input[name="prompt"]');

  function readHistory() {
    try {
      var raw = window.localStorage.getItem(key);
      var parsed = raw ? JSON.parse(raw) : [];
      if (!Array.isArray(parsed)) return [];
      return parsed.filter(function (item) {
        return typeof item === "string" && item.trim() !== "";
      });
    } catch (err) {
      return [];
    }
  }

  function writeHistory(items) {
    try {
      window.localStorage.setItem(key, JSON.stringify(items.slice(0, maxItems)));
    } catch (err) {
      // Local history is optional; submitting the question should still work.
    }
  }

  function saveQuestion(question) {
    question = (question || "").trim();
    if (!question) return;
    var lowered = question.toLowerCase();
    var items = readHistory().filter(function (item) {
      return item.toLowerCase() !== lowered;
    });
    items.unshift(question);
    writeHistory(items);
  }

  function rerunQuestion(question) {
    question = (question || "").trim();
    if (!question) return;
    saveQuestion(question);
    showGlobalBusy(i18n("Asking..."));

    var form = document.createElement("form");
    form.method = "post";
    form.action = "/assistant/ask";

    var input = document.createElement("input");
    input.type = "hidden";
    input.name = "prompt";
    input.value = question;
    form.appendChild(input);

    document.body.appendChild(form);
    submitWithBusy(form, i18n("Asking..."));
  }

  function renderHistory() {
    var items = readHistory();
    list.innerHTML = "";
    if (!items.length) {
      var empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = i18n("No questions yet.");
      list.appendChild(empty);
      return;
    }
    var current = promptInput ? promptInput.value.trim().toLowerCase() : "";
    items.forEach(function (question) {
      var row = document.createElement("div");
      row.className = "assistant-history-item";

      var run = document.createElement("button");
      run.type = "button";
      run.className = "assistant-history-run";
      if (current && question.toLowerCase() === current) run.classList.add("selected");
      run.textContent = question;
      run.title = question;
      run.dataset.question = question;
      row.appendChild(run);

      var del = document.createElement("button");
      del.type = "button";
      del.className = "assistant-history-delete";
      del.textContent = "x";
      del.title = i18n("Delete question");
      del.setAttribute("aria-label", i18n("Delete question"));
      del.dataset.question = question;
      row.appendChild(del);

      list.appendChild(row);
    });
  }

  document.querySelectorAll("[data-assistant-prompt-form]").forEach(function (form) {
    form.addEventListener("submit", function () {
      var input = form.querySelector('input[name="prompt"]');
      if (input) saveQuestion(input.value);
    });
  });

  function showFreshAskPage() {
    try {
      window.localStorage.setItem("mmxc:last:ask", "/assistant");
    } catch (err) {}
    navigateWithBusy("/assistant", false);
  }

  function resetWhenQuestionCleared() {
    if (!promptInput || promptInput.value.trim() !== "") return;
    if (window.location.pathname !== "/assistant") return;
    if (!window.location.search && !document.querySelector(".assistant-thread, .table-wrap, .query-details")) return;
    showFreshAskPage();
  }

  if (promptInput) {
    promptInput.addEventListener("input", resetWhenQuestionCleared);
    promptInput.addEventListener("search", resetWhenQuestionCleared);
  }

  list.addEventListener("click", function (event) {
    if (!event.target.closest) return;
    var deleteButton = event.target.closest(".assistant-history-delete");
    if (deleteButton) {
      var remove = (deleteButton.dataset.question || "").toLowerCase();
      var items = readHistory().filter(function (item) {
        return item.toLowerCase() !== remove;
      });
      writeHistory(items);
      renderHistory();
      return;
    }
    var runButton = event.target.closest(".assistant-history-run");
    if (runButton) rerunQuestion(runButton.dataset.question || "");
  });

  // The quick-start samples ask exactly like a history entry, so a clicked
  // sample also lands in the local history. Their label is the question.
  var samples = document.querySelector("[data-assistant-samples]");
  if (samples) {
    samples.addEventListener("click", function (event) {
      if (!event.target.closest) return;
      var sample = event.target.closest(".assistant-sample");
      if (sample) rerunQuestion(sample.textContent || "");
    });
  }

  renderHistory();
})();

document.querySelectorAll("[data-candidate-row]").forEach(function (row) {
  updateCandidateRow(row);
});
updateDuplicateVisibility();
updateClosingBanner();
updateImportButton();

document.querySelectorAll("[data-statement-upload]").forEach(function (form) {
  var input = form.querySelector('input[type="file"][name="statement"]');
  var busy = form.querySelector(".statement-busy");
  if (input) {
    input.addEventListener("change", function () {
      if (input.files && input.files.length) form.requestSubmit();
    });
  }
  form.addEventListener("dragover", function (event) {
    event.preventDefault();
    form.classList.add("drag-over");
  });
  form.addEventListener("dragleave", function () {
    form.classList.remove("drag-over");
  });
  form.addEventListener("drop", function (event) {
    event.preventDefault();
    form.classList.remove("drag-over");
    if (!input || !event.dataTransfer || !event.dataTransfer.files.length) return;
    input.files = event.dataTransfer.files;
    form.requestSubmit();
  });
  form.addEventListener("submit", function () {
    form.dataset.busy = "true";
    if (busy) busy.hidden = false;
    showGlobalBusy(i18n("Processing statement..."));
  });
});

document.querySelectorAll("[data-auto-submit]").forEach(function (form) {
  form.querySelectorAll("select").forEach(function (select) {
    select.addEventListener("change", function () {
      form.requestSubmit();
    });
  });
});

function updateDuplicateVisibility() {
  var table = document.querySelector(".candidate-table");
  var toggle = document.querySelector("[data-hide-duplicates]");
  if (!table || !toggle) return;
  table.classList.toggle("hide-duplicates", toggle.checked);
}

document.addEventListener("change", function (event) {
  // Settle a typed calculation into its result before anything reads the field:
  // the totals below, the budget autosave, and the submitted value all want the
  // number rather than the expression.
  if (event.target.matches && event.target.matches("[data-amount]")) {
    normalizeAmountInput(event.target);
    markAmountValidity(event.target);
  }
  var form = event.target.closest ? event.target.closest("[data-transaction-form]") : null;
  if (form) updateTransactionForm(form);
  var candidateRow = event.target.closest ? event.target.closest("[data-candidate-row]") : null;
  if (candidateRow) updateCandidateRow(candidateRow, event.target.name === "approved");
  if (event.target.matches && event.target.matches("[data-budget-autosave]") && event.target.form) {
    event.target.form.requestSubmit();
  }
  if (event.target.matches && event.target.matches("[data-hide-duplicates]")) updateDuplicateVisibility();
});

document.addEventListener("input", function (event) {
  // Typing towards a valid amount clears the flag immediately; only leaving the
  // field (the change handler) can set it.
  if (event.target.matches && event.target.matches("[data-amount]")) event.target.classList.remove("amount-invalid");
  var splitEditor = event.target.closest ? event.target.closest("[data-split-editor]") : null;
  if (splitEditor && /^split_amount/.test(event.target.name || "")) {
    refreshSplitEditor(splitEditor);
    return;
  }
  var candidateRow = event.target.closest ? event.target.closest("[data-candidate-row]") : null;
  if (candidateRow) updateCandidateRow(candidateRow);
});

// Submitting with Enter can skip the change event that would have settled a
// calculation, so evaluate every amount the form owns on the way out. Uses
// form.elements because the inline budget rows attach their inputs by form id
// rather than by nesting.
document.addEventListener("submit", function (event) {
  var form = event.target;
  if (!form || !form.elements) return;
  Array.prototype.forEach.call(form.elements, function (control) {
    if (control.matches && control.matches("[data-amount]")) normalizeAmountInput(control);
  });
}, true);

function focusCandidateRow(row) {
  if (!row) return;
  row.scrollIntoView({ block: "nearest" });
  var date = row.querySelector('input[name="date"]');
  if (date) date.focus({ preventScroll: true });
}

function dismissReviewCandidate(button) {
  var form = button.form;
  var row = button.closest("[data-candidate-row]");
  if (!form || !row) return;

  var rows = Array.prototype.slice.call(form.querySelectorAll("[data-candidate-row]"));
  var dismissedIndex = rows.indexOf(row);
  var formData = new FormData(form);
  formData.set(button.name, button.value);
  var body = new URLSearchParams();
  formData.forEach(function (value, name) {
    body.append(name, value);
  });
  button.disabled = true;
  row.setAttribute("aria-busy", "true");

  fetch(button.getAttribute("formaction") || form.action, {
    method: "POST",
    body: body,
    headers: { "X-MMXC-Partial": "true" }
  }).then(function (response) {
    if (!response.ok) throw new Error("dismiss failed");

    row.remove();
    updateClosingBanner();
    updateImportButton();

    var remaining = Array.prototype.slice.call(form.querySelectorAll("[data-candidate-row]"));
    if (!remaining.length) {
      var tbody = form.querySelector(".candidate-table tbody");
      var emptyRow = document.createElement("tr");
      var emptyCell = document.createElement("td");
      emptyCell.colSpan = 8;
      emptyCell.className = "empty";
      emptyCell.tabIndex = -1;
      emptyCell.textContent = i18n("No statement candidates remain.");
      emptyRow.appendChild(emptyCell);
      if (tbody) tbody.appendChild(emptyRow);
      var toggle = form.querySelector(".candidate-toggle");
      var submit = form.querySelector('button[type="submit"]:not([data-dismiss-candidate])');
      if (toggle) toggle.hidden = true;
      if (submit) submit.hidden = true;
      emptyCell.scrollIntoView({ block: "nearest" });
      emptyCell.focus({ preventScroll: true });
      return;
    }

    var next = null;
    for (var i = dismissedIndex; i < rows.length; i++) {
      if (rows[i] !== row && rows[i].isConnected && rows[i].offsetParent !== null) {
        next = rows[i];
        break;
      }
    }
    if (!next) {
      for (var j = dismissedIndex - 1; j >= 0; j--) {
        if (rows[j].isConnected && rows[j].offsetParent !== null) {
          next = rows[j];
          break;
        }
      }
    }
    if (next) {
      focusCandidateRow(next);
    } else {
      var duplicateToggle = form.querySelector("[data-hide-duplicates]");
      if (duplicateToggle) {
        duplicateToggle.scrollIntoView({ block: "nearest" });
        duplicateToggle.focus({ preventScroll: true });
      }
    }
  }).catch(function () {
    button.disabled = false;
    row.removeAttribute("aria-busy");
    appAlert(i18n("Could not dismiss candidate. Please try again."));
  });
}

document.addEventListener("click", function (event) {
  var dismiss = event.target.closest ? event.target.closest("[data-dismiss-candidate]") : null;
  if (dismiss) {
    // Dismissal is normally an in-place fetch; without it the button falls back
    // to submitting its form. Both paths now run after the confirm dialog
    // resolves, so the click is always cancelled first and replayed on accept.
    var inPlace = !!(window.fetch && window.FormData && window.URLSearchParams);
    var run = function () {
      if (inPlace) {
        dismissReviewCandidate(dismiss);
        return;
      }
      var form = dismiss.form;
      if (!form) return;
      if (form.requestSubmit) form.requestSubmit(dismiss); else form.submit();
    };
    event.preventDefault();
    if (!dismiss.dataset.confirm) {
      run();
      return;
    }
    appConfirm(dismiss.dataset.confirm, { ok: dismiss.dataset.confirmOk }).then(function (accepted) {
      if (accepted) run();
    });
    return;
  }
  var add = event.target.closest ? event.target.closest("[data-split-add]") : null;
  if (add) {
    cloneSplitLine(add.closest("[data-split-editor]"));
    return;
  }
  var remove = event.target.closest ? event.target.closest("[data-split-remove]") : null;
  if (remove) {
    var line = remove.closest(".split-line");
    var editor = remove.closest("[data-split-editor]");
    var lines = editor ? editor.querySelectorAll(".split-line") : [];
    if (lines.length > 2) {
      line.remove();
    } else {
      line.querySelectorAll("input").forEach(function (input) {
        input.value = "";
      });
    }
    refreshSplitEditor(editor);
  }
});

(function () {
  function cleanModuleURL(value) {
    var url = new URL(value, window.location.origin);
    url.searchParams.delete("edit_trans_id");
    url.searchParams.delete("scroll");
    url.searchParams.delete("highlight");
    return url.pathname + url.search;
  }
  var active = document.querySelector(".pills a.active");
  if (active) {
    try {
      localStorage.setItem("mmxc:last:" + active.textContent.trim().toLowerCase(), cleanModuleURL(window.location.pathname + window.location.search));
    } catch (error) {}
  }
  document.querySelectorAll(".pills a").forEach(function (link) {
    link.addEventListener("click", function (event) {
      try {
        var saved = localStorage.getItem("mmxc:last:" + link.textContent.trim().toLowerCase());
        var target = new URL(link.getAttribute("href"), window.location.origin);
        var savedURL = saved ? new URL(saved, window.location.origin) : null;
        if (savedURL && savedURL.pathname === target.pathname && saved !== target.pathname) {
          event.preventDefault();
          navigateWithBusy(saved, false);
        }
      } catch (error) {}
    });
  });
})();

document.addEventListener("click", function (event) {
  var row = event.target.closest ? event.target.closest("tbody tr") : null;
  if (!row || row.classList.contains("edit-row") || event.target.closest("a, button, input, select, textarea, label")) return;
  var table = row.closest("table");
  if (!table) return;
  table.querySelectorAll("tr.row-selected").forEach(function (selected) {
    selected.classList.remove("row-selected");
  });
  row.classList.add("row-selected");
});

document.addEventListener("keydown", function (event) {
  if (event.key !== "Escape") return;
  // An open dialog handles its own Escape; don't also cancel an edit row or a
  // details panel on the page behind it.
  if (topDialog()) return;
  var editCancel = document.querySelector(".edit-row .button-link");
  if (editCancel) {
    navigateWithBusy(editCancel.getAttribute("href"), false);
    return;
  }
  var openPanels = Array.prototype.slice.call(document.querySelectorAll("details[open]"));
  if (openPanels.length) {
    openPanels[openPanels.length - 1].open = false;
  }
});

(function () {
  var menu = document.getElementById("row-menu");
  if (!menu) return;
  var current = null;

  function hide() {
    menu.hidden = true;
    current = null;
    if (menu.parentNode !== document.body) document.body.appendChild(menu);
  }

  document.addEventListener("contextmenu", function (event) {
    var row = event.target.closest ? event.target.closest("tr.actionable") : null;
    if (!row) {
      hide();
      return;
    }
    event.preventDefault();
    current = row;
    // A voided row is only actionable when permanent deletion is enabled, and
    // then deleting is all it offers: it can be neither edited nor re-voided.
    var voided = row.dataset.voided === "true";
    menu.querySelectorAll('[data-action="edit"], [data-action="void"]').forEach(function (button) {
      button.parentNode.hidden = voided;
    });
    // Report rows are also actionable inside a report detail modal. A modal
    // dialog puts everything outside itself in the top layer's shadow — both
    // painted under it and inert — so the menu is re-parented into the open
    // dialog while it is up. It stays position:fixed, so nothing about its
    // placement changes; hide() puts it back on <body>.
    var host = topDialog() || document.body;
    if (menu.parentNode !== host) host.appendChild(menu);
    menu.hidden = false;
    var x = event.clientX;
    var y = event.clientY;
    var rect = menu.getBoundingClientRect();
    if (x + rect.width > window.innerWidth) x = window.innerWidth - rect.width - 4;
    if (y + rect.height > window.innerHeight) y = window.innerHeight - rect.height - 4;
    menu.style.left = Math.max(x, 4) + "px";
    menu.style.top = Math.max(y, 4) + "px";
  });

  menu.addEventListener("click", function (event) {
    var action = event.target.closest("[data-action]");
    if (!action || !current) return;
    var transId = current.dataset.transId;
    var accountId = current.dataset.accountId;
    var returnTo = current.dataset.returnTo || "";
    var scroll = String(Math.round(window.scrollY));
    if (action.dataset.action === "edit") {
      // editUrl, when present, points at a page that can render the edit form
      // (e.g. anomalies rows send the user to /accounts) while returnTo carries
      // where to land after saving.
      var editUrl = current.dataset.editUrl;
      var base = editUrl || returnTo || ("/accounts?account_id=" + encodeURIComponent(accountId));
      var next = new URL(base, window.location.origin);
      next.searchParams.set("edit_trans_id", transId);
      next.searchParams.set("scroll", scroll);
      if (editUrl && returnTo) next.searchParams.set("return_to", returnTo);
      navigateWithBusy(next.pathname + next.search, false);
    } else if (action.dataset.action === "void") {
      // The menu closes first: the confirm dialog resolves asynchronously, and
      // leaving the menu up behind it would strand it there on cancel. The row
      // identifiers are already captured above, so losing `current` is fine.
      hide();
      // "Void transaction", not a bare "Void": several languages render both
      // "Void" and "Cancel" as the same word, which would put two identically
      // labelled buttons side by side in the dialog.
      appConfirm(i18n("Void this transaction?"), { ok: i18n("Void transaction") }).then(function (accepted) {
        if (accepted) postRowAction("/transactions/void", accountId, transId, scroll, returnTo);
      });
      return;
    } else if (action.dataset.action === "delete") {
      hide();
      appConfirm(i18n("Permanently delete this transaction? This cannot be undone."), { ok: i18n("Delete transaction") }).then(function (accepted) {
        if (accepted) postRowAction("/transactions/delete", accountId, transId, scroll, returnTo);
      });
      return;
    }
    hide();
  });

  function postRowAction(path, accountId, transId, scroll, returnTo) {
    var form = document.createElement("form");
    form.method = "post";
    form.action = path;
    form.appendChild(hiddenInput("account_id", accountId));
    form.appendChild(hiddenInput("trans_id", transId));
    form.appendChild(hiddenInput("scroll", scroll));
    form.appendChild(hiddenInput("return_to", returnTo));
    document.body.appendChild(form);
    submitWithBusy(form);
  }

  function hiddenInput(name, value) {
    var input = document.createElement("input");
    input.type = "hidden";
    input.name = name;
    input.value = value;
    return input;
  }

  document.addEventListener("click", function (event) {
    if (!menu.hidden && !menu.contains(event.target)) hide();
  });
  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") hide();
  });
  window.addEventListener("resize", hide);
  window.addEventListener("scroll", hide, true);
})();

// Filter toolbars (Accounts / Categories / Payees) auto-submit, so there is no
// Apply button. Selects and the From/To date fields submit on change (the latter
// fire on blur); the free-text Search/Payee/Category fields submit on a long
// debounce. The chosen filters also persist per section (localStorage), so they
// survive switching the selected entity, reloading, or returning via the nav.
(function () {
  // "limit" only exists on Categories/Payees, whose toolbars cap the list at the
  // latest N instead of defaulting to a date window; Accounts simply never sends it.
  var FIELDS = ["from", "to", "type", "status", "payee", "category", "search", "limit"];
  var SECTIONS = { "/accounts": true, "/categories": true, "/payees": true };
  var path = window.location.pathname;

  if (SECTIONS[path]) {
    // v2: the Categories/Payees defaults changed from a date window to a count,
    // so filters saved by an older build are dropped rather than restored.
    var key = "mmxc.filters.v2:" + path;
    var params = new URLSearchParams(window.location.search);

    if (params.has("reset")) {
      // The Clear link forgets the saved filters and returns to the default view.
      try {
        localStorage.removeItem(key);
      } catch (e) {}
      FIELDS.forEach(function (f) {
        params.delete(f);
      });
      params.delete("reset");
      var resetQs = params.toString();
      navigateWithBusy(path + (resetQs ? "?" + resetQs : ""), true);
      return;
    }

    var effective = {};
    if (FIELDS.some(function (f) { return params.has(f); })) {
      // The URL carries filters: remember exactly what is shown for next time.
      FIELDS.forEach(function (f) {
        if (params.has(f)) effective[f] = params.get(f);
      });
      try {
        localStorage.setItem(key, JSON.stringify(effective));
      } catch (e) {}
    } else {
      // Arrived without filters (reload of a bare URL, or the top nav): restore
      // the saved ones with a single redirect.
      try {
        effective = JSON.parse(localStorage.getItem(key) || "{}") || {};
      } catch (e) {
        effective = {};
      }
      var savedKeys = Object.keys(effective);
      if (savedKeys.length) {
        savedKeys.forEach(function (f) {
          params.set(f, effective[f]);
        });
        navigateWithBusy(path + "?" + params.toString(), true);
        return;
      }
    }

    // Carry the active filters onto the sidebar links so switching the selected
    // account/category/payee keeps them without an extra round-trip.
    var suffix = "";
    Object.keys(effective).forEach(function (f) {
      suffix += "&" + encodeURIComponent(f) + "=" + encodeURIComponent(effective[f]);
    });
    if (suffix) {
      document.querySelectorAll("a.entity-link, a.account-link").forEach(function (a) {
        a.setAttribute("href", a.getAttribute("href") + suffix);
      });
    }
  }

  function closestAutosubmit(el) {
    var form = el && el.form;
    return form && form.classList.contains("autosubmit") ? form : null;
  }

  document.addEventListener("change", function (event) {
    var el = event.target;
    var form = closestAutosubmit(el);
    if (!form) return;
    if (el.tagName === "SELECT" || el.name === "from" || el.name === "to") {
      submitWithBusy(form);
    }
  });

  var timer;
  document.addEventListener("input", function (event) {
    var el = event.target;
    var form = closestAutosubmit(el);
    if (!form || el.type !== "search") return;
    clearTimeout(timer);
    timer = setTimeout(function () {
      submitWithBusy(form);
    }, 1500);
  });
})();

// Reports range: each report remembers its own From/To in localStorage, so the
// nav links carry only ?report=ID and the saved range is restored on arrival.
// The "Past N months" / "Full year" shortcuts compute precise period borders in
// the granularity the current report's picker expects, then submit.
(function () {
  if (window.location.pathname !== "/reports") return;

  var FIELDS = ["from", "to", "top_n"];
  var params = new URLSearchParams(window.location.search);
  var reportID = params.get("report") || "overview";
  var key = "mmxc.reports:" + reportID;

  if (params.has("reset")) {
    try { localStorage.removeItem(key); } catch (e) {}
    navigateWithBusy("/reports?report=" + encodeURIComponent(reportID), true);
    return;
  }

  if (FIELDS.some(function (f) { return params.has(f); })) {
    var saved = {};
    FIELDS.forEach(function (f) { if (params.has(f)) saved[f] = params.get(f); });
    try { localStorage.setItem(key, JSON.stringify(saved)); } catch (e) {}
  } else {
    var restored = {};
    try { restored = JSON.parse(localStorage.getItem(key) || "{}") || {}; } catch (e) {}
    if (Object.keys(restored).length) {
      Object.keys(restored).forEach(function (f) { params.set(f, restored[f]); });
      navigateWithBusy("/reports?" + params.toString(), true);
      return;
    }
  }

  var form = document.querySelector("form.reports-toolbar");
  if (!form) return;
  var gran = form.getAttribute("data-granularity");
  var fromEl = form.querySelector("[name=from]");
  var toEl = form.querySelector("[name=to]");

  function pad(n) {
    return (n < 10 ? "0" : "") + n;
  }
  function fmt(d) {
    var ym = d.getFullYear() + "-" + pad(d.getMonth() + 1);
    if (gran === "year") return "" + d.getFullYear();
    if (gran === "month") return ym;
    return ym + "-" + pad(d.getDate());
  }
  function applyRange(start, end) {
    if (fromEl) fromEl.value = fmt(start);
    if (toEl) toEl.value = fmt(end);
    submitWithBusy(form);
  }

  var yearSel = form.querySelector("[data-range-year]");
  if (yearSel) {
    yearSel.addEventListener("change", function () {
      if (!this.value) return;
      var y = parseInt(this.value, 10);
      applyRange(new Date(y, 0, 1), new Date(y, 11, 31));
    });
  }

  var monthsSel = form.querySelector("[data-range-months]");
  if (monthsSel) {
    monthsSel.addEventListener("change", function () {
      if (!this.value) return;
      var n = parseInt(this.value, 10);
      var now = new Date();
      // "Past N months" means the last N *completed* months: the current month
      // is still in progress, so including it would understate every total.
      // In July, "3" selects April 1 – June 30.
      var end = new Date(now.getFullYear(), now.getMonth(), 0); // last day of previous month
      var start = new Date(now.getFullYear(), now.getMonth() - n, 1);
      applyRange(start, end);
    });
  }

  form.querySelectorAll("[data-report-submit]").forEach(function (el) {
    el.addEventListener("change", function () { submitWithBusy(form); });
  });
})();

// After the transaction editor closes (save or cancel) the page lands with a
// highlight=<id> param so the just-edited row is re-selected. This runs after
// the filter-restore block above: if that block does a location.replace to
// re-apply saved filters (e.g. on the filter-less cancel URL), highlight rides
// along in the params and is applied on the reloaded page. Strip it afterward
// so a manual reload or pills-nav doesn't keep re-selecting the row.
(function () {
  var match = /[?&]highlight=(\d+)/.exec(window.location.search);
  if (!match) return;
  var row = document.querySelector('tr[data-trans-id="' + match[1] + '"]');
  if (row) {
    var table = row.closest("table");
    if (table) {
      table.querySelectorAll("tr.row-selected").forEach(function (selected) {
        selected.classList.remove("row-selected");
      });
    }
    row.classList.add("row-selected");
  }
  try {
    var url = new URL(window.location.href);
    url.searchParams.delete("highlight");
    window.history.replaceState(null, "", url.pathname + url.search + url.hash);
  } catch (error) {}
})();
