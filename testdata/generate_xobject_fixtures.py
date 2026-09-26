"""
Generates PDF fixtures that exercise Form XObject recursion (Track A
Priority 1). reportlab's beginForm/doForm API produces genuine PDF Form
XObjects (verified: real /Subtype /Form dictionaries with a real Do
operator in the content stream, not an inlined approximation), which is
exactly the "hidden data block in a reusable template" scenario the
priority targets.

Run from the repo root: python3 testdata/generate_xobject_fixtures.py
"""
import io
from reportlab.pdfgen import canvas
from reportlab.lib.pagesizes import letter
from reportlab.lib.utils import ImageReader

OUT_DIR = "testdata"


def generate_single_form(path):
    """One Form XObject containing an invoice's hidden subtotal/tax/total
    block, invoked once via doForm. This is the base case: without Form
    XObject recursion, extraction sees only the page's own direct content
    ("Invoice INV-2026-0088") and completely misses the form's contents."""
    c = canvas.Canvas(path, pagesize=letter)
    c.beginForm("invoicefooter", lowerx=0, lowery=0, upperx=300, uppery=100)
    c.setFont("Helvetica", 10)
    c.drawString(5, 70, "Subtotal: 4,200.00")
    c.drawString(5, 50, "Tax (8.25%): 346.50")
    c.drawString(5, 30, "Total Due: 4,546.50")
    c.endForm()

    c.setFont("Helvetica-Bold", 14)
    c.drawString(72, 700, "Invoice INV-2026-0088")
    c.saveState()
    c.translate(72, 500)
    c.doForm("invoicefooter")
    c.restoreState()
    c.showPage()
    c.save()
    print(f"wrote {path}")


def generate_nested_form_and_image(path):
    """A Form XObject that itself invokes a second, nested Form XObject
    (recursion depth 2), plus a real Image XObject drawn via Do (to confirm
    Image XObjects are recognized and skipped rather than crashing or
    being misparsed as a content stream)."""
    c = canvas.Canvas(path, pagesize=letter)

    c.beginForm("innerform", lowerx=0, lowery=0, upperx=250, uppery=60)
    c.setFont("Helvetica", 9)
    c.drawString(5, 40, "Line item: Widget A x 12 @ 4.50 = 54.00")
    c.drawString(5, 20, "Line item: Widget B x 3 @ 19.99 = 59.97")
    c.endForm()

    c.beginForm("outerform", lowerx=0, lowery=0, upperx=280, uppery=100)
    c.setFont("Helvetica-Bold", 10)
    c.drawString(5, 85, "Line Items (nested template)")
    c.saveState()
    c.translate(10, 10)
    c.doForm("innerform")
    c.restoreState()
    c.endForm()

    c.setFont("Helvetica-Bold", 14)
    c.drawString(72, 700, "Purchase Order PO-4471")
    c.saveState()
    c.translate(72, 550)
    c.doForm("outerform")
    c.restoreState()

    # 10x10 solid-color PNG, drawn as a real Image XObject.
    from PIL import Image as PILImage
    img = PILImage.new("RGB", (10, 10), color=(200, 50, 50))
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    buf.seek(0)
    c.drawImage(ImageReader(buf), 400, 700, width=30, height=30)

    c.showPage()
    c.save()
    print(f"wrote {path}")


if __name__ == "__main__":
    generate_single_form(f"{OUT_DIR}/xobject_single_form.pdf")
    generate_nested_form_and_image(f"{OUT_DIR}/xobject_nested_form_and_image.pdf")
